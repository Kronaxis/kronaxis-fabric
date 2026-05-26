// fabric-watcher: inotify-driven incremental code-graph reindex.
//
// Watches each configured repo recursively. When a tracked source file is
// written/created/removed, after a per-file debounce window we:
//
//  1. POST /v1/symbol/reindex   (soft-delete previous symbols for that file)
//  2. Re-invoke the indexer with --file <rel> --root <repo path>
//     so symbols are repopulated. Removals stop at step 1.
//
// Rename is handled as delete-old + insert-new (fsnotify Rename event on the
// old path → soft-delete; Create event on the new path → reindex).
//
// On boot we walk every configured repo once so anything that changed while
// the watcher was down gets reconciled.
//
// On HTTP 5xx we never drop events — failed pending changes are re-queued
// with exponential backoff (cap 60s).
//
// Metrics: GET /healthz on metrics_addr returns JSON with last_event_at,
// events_since_start, failed_upserts_since_start, watched_paths_count.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

const watcherVersion = "0.9.0"

type repoCfg struct {
	Repo      string   `yaml:"repo"`
	Path      string   `yaml:"path"`
	Languages []string `yaml:"languages"`
}

type config struct {
	FabricURL     string    `yaml:"fabric_url"`
	FabricKey     string    `yaml:"fabric_key"`
	IndexerPath   string    `yaml:"indexer_path"`
	Repos         []repoCfg `yaml:"repos"`
	DebounceMs    int       `yaml:"debounce_ms"`
	Ignore        []string  `yaml:"ignore"`
	MetricsAddr   string    `yaml:"metrics_addr"`
	BackoffMaxMs  int       `yaml:"backoff_max_ms"`
	StartupRescan *bool     `yaml:"startup_rescan"`
}

type pendingChange struct {
	repo     string
	root     string
	rel      string
	deleted  bool
	deadline time.Time
	attempts int // retry counter for exponential backoff
}

// metrics is exposed on /healthz; all fields are atomically updated.
type metrics struct {
	lastEventAt              atomic.Int64 // unix nanoseconds; 0 = none
	eventsSinceStart         atomic.Uint64
	failedUpsertsSinceStart  atomic.Uint64
	successfulUpsertsSince   atomic.Uint64
	watchedPathsCount        atomic.Int64
	pendingDepth             atomic.Int64
	startedAt                time.Time
}

type watcher struct {
	cfg     *config
	verbose bool

	mu      sync.Mutex
	pending map[string]*pendingChange // key: repo + "\x00" + rel

	fsn     *fsnotify.Watcher
	http    *http.Client
	repos   map[string]*repoCfg        // path → repo
	exts    map[string]map[string]bool // repo → set of extensions
	metrics metrics
}

func loadConfig(path string) (*config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	raw = []byte(os.ExpandEnv(string(raw)))
	c := &config{}
	if err := yaml.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("yaml %s: %w", path, err)
	}
	if c.DebounceMs <= 0 {
		c.DebounceMs = 500
	}
	if c.BackoffMaxMs <= 0 {
		c.BackoffMaxMs = 60_000
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = "127.0.0.1:8202"
	}
	if c.FabricURL == "" {
		c.FabricURL = "http://localhost:8201"
	}
	if c.FabricKey == "" {
		c.FabricKey = os.Getenv("FABRIC_KEY")
	}
	if c.IndexerPath == "" {
		return nil, errors.New("indexer_path required in config")
	}
	if len(c.Repos) == 0 {
		return nil, errors.New("no repos configured")
	}
	if len(c.Ignore) == 0 {
		c.Ignore = []string{".git/", "node_modules/", "vendor/", "target/", "dist/", "__pycache__/", "*.pyc"}
	}
	c.FabricURL = strings.TrimRight(c.FabricURL, "/")
	return c, nil
}

var langExt = map[string][]string{
	"go":     {".go"},
	"python": {".py"},
	"ts":     {".ts", ".tsx"},
	"js":     {".js", ".jsx"},
}

func buildExtMap(repos []repoCfg) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range repos {
		set := map[string]bool{}
		for _, lang := range r.Languages {
			for _, e := range langExt[strings.ToLower(lang)] {
				set[e] = true
			}
		}
		out[r.Repo] = set
	}
	return out
}

// matchesIgnore returns true if rel matches any ignore pattern. Patterns
// ending in "/" match any path segment of that name; other patterns are
// glob-matched against the file's basename.
func matchesIgnore(rel string, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			needle := strings.TrimSuffix(p, "/")
			parts := strings.Split(rel, string(filepath.Separator))
			for _, part := range parts {
				if part == needle {
					return true
				}
			}
		} else {
			if matched, _ := filepath.Match(p, filepath.Base(rel)); matched {
				return true
			}
		}
	}
	return false
}

func (w *watcher) trackedExt(repo, rel string) bool {
	ext := strings.ToLower(filepath.Ext(rel))
	return w.exts[repo][ext]
}

func (w *watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if w.verbose {
				log.Printf("walk warn %s: %v", p, err)
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && matchesIgnore(rel, w.cfg.Ignore) {
			return filepath.SkipDir
		}
		if err := w.fsn.Add(p); err != nil {
			log.Printf("fsnotify add %s warn: %v", p, err)
		} else {
			w.metrics.watchedPathsCount.Add(1)
			if w.verbose {
				log.Printf("watch + %s", p)
			}
		}
		return nil
	})
}

func (w *watcher) resolveRepo(absPath string) (*repoCfg, string) {
	for path, r := range w.repos {
		if strings.HasPrefix(absPath, path+string(filepath.Separator)) || absPath == path {
			rel, err := filepath.Rel(path, absPath)
			if err == nil {
				return r, rel
			}
		}
	}
	return nil, ""
}

func (w *watcher) enqueue(repo, root, rel string, deleted bool) {
	key := repo + "\x00" + rel
	w.mu.Lock()
	defer w.mu.Unlock()
	// Preserve attempts if there's already a pending change for this file —
	// the new event supersedes the old one but inherits its backoff state
	// so a single thrashing file doesn't reset its retry counter on each save.
	attempts := 0
	if prev, ok := w.pending[key]; ok {
		attempts = prev.attempts
	}
	w.pending[key] = &pendingChange{
		repo:     repo,
		root:     root,
		rel:      rel,
		deleted:  deleted,
		deadline: time.Now().Add(time.Duration(w.cfg.DebounceMs) * time.Millisecond),
		attempts: attempts,
	}
	w.metrics.pendingDepth.Store(int64(len(w.pending)))
}

// requeueWithBackoff re-inserts a failed change with exponential delay
// (base 1s, doubling, cap BackoffMaxMs). Never drops events.
func (w *watcher) requeueWithBackoff(p *pendingChange) {
	p.attempts++
	delayMs := 1000 << (p.attempts - 1)
	if delayMs > w.cfg.BackoffMaxMs {
		delayMs = w.cfg.BackoffMaxMs
	}
	p.deadline = time.Now().Add(time.Duration(delayMs) * time.Millisecond)
	key := p.repo + "\x00" + p.rel
	w.mu.Lock()
	defer w.mu.Unlock()
	// If another event arrived in the meantime, that newer event wins, but
	// we lift its attempts counter so the chain doesn't restart at zero.
	if existing, ok := w.pending[key]; ok {
		if existing.attempts < p.attempts {
			existing.attempts = p.attempts
		}
		w.metrics.pendingDepth.Store(int64(len(w.pending)))
		return
	}
	w.pending[key] = p
	w.metrics.pendingDepth.Store(int64(len(w.pending)))
}

func (w *watcher) drainOnce(ctx context.Context) int {
	now := time.Now()
	w.mu.Lock()
	ready := make([]*pendingChange, 0, len(w.pending))
	for k, p := range w.pending {
		if !now.Before(p.deadline) {
			ready = append(ready, p)
			delete(w.pending, k)
		}
	}
	w.metrics.pendingDepth.Store(int64(len(w.pending)))
	w.mu.Unlock()
	for _, p := range ready {
		w.process(ctx, p)
	}
	return len(ready)
}

func (w *watcher) drainAll(ctx context.Context) int {
	w.mu.Lock()
	ready := make([]*pendingChange, 0, len(w.pending))
	for k, p := range w.pending {
		ready = append(ready, p)
		delete(w.pending, k)
	}
	w.metrics.pendingDepth.Store(0)
	w.mu.Unlock()
	for _, p := range ready {
		w.process(ctx, p)
	}
	return len(ready)
}

// process clears the file's previous symbols, then either stops (delete) or
// re-runs the indexer for that single file. On 5xx the change is requeued
// with exponential backoff rather than dropped.
func (w *watcher) process(ctx context.Context, p *pendingChange) {
	status, err := w.postReindex(ctx, p.repo, p.rel)
	if err != nil {
		if status >= 500 || status == 0 {
			// Transient: requeue.
			w.metrics.failedUpsertsSinceStart.Add(1)
			log.Printf("reindex %s:%s transient err (status=%d attempts=%d): %v — requeue", p.repo, p.rel, status, p.attempts+1, err)
			w.requeueWithBackoff(p)
			return
		}
		// Permanent (4xx, malformed body, etc): log and drop this update.
		w.metrics.failedUpsertsSinceStart.Add(1)
		log.Printf("reindex %s:%s permanent err (status=%d): %v — drop", p.repo, p.rel, status, err)
		return
	}
	if w.verbose {
		log.Printf("reindex %s:%s cleared", p.repo, p.rel)
	}
	if p.deleted {
		w.metrics.successfulUpsertsSince.Add(1)
		log.Printf("removed %s:%s", p.repo, p.rel)
		return
	}
	if err := w.invokeIndexer(ctx, p.repo, p.root, p.rel); err != nil {
		w.metrics.failedUpsertsSinceStart.Add(1)
		log.Printf("indexer %s:%s warn (attempts=%d): %v — requeue", p.repo, p.rel, p.attempts+1, err)
		w.requeueWithBackoff(p)
		return
	}
	w.metrics.successfulUpsertsSince.Add(1)
	log.Printf("updated %s:%s", p.repo, p.rel)
}

// postReindex returns (httpStatus, error). httpStatus is 0 on network error.
func (w *watcher) postReindex(ctx context.Context, repo, rel string) (int, error) {
	body, _ := json.Marshal(map[string]string{"repo": repo, "file_path": rel})
	req, err := http.NewRequestWithContext(ctx, "POST", w.cfg.FabricURL+"/v1/symbol/reindex", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.FabricKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return 200, nil
}

func (w *watcher) invokeIndexer(ctx context.Context, repo, root, rel string) error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "python3", w.cfg.IndexerPath,
		"--repo", repo,
		"--root", root,
		"--file", rel,
	)
	cmd.Env = append(os.Environ(),
		"FABRIC_URL="+w.cfg.FabricURL,
		"FABRIC_KEY="+w.cfg.FabricKey,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	if w.verbose && len(out) > 0 {
		log.Printf("indexer %s:%s → %s", repo, rel, strings.TrimSpace(string(out)))
	}
	return nil
}

// startupRescan walks every configured repo and enqueues every tracked file.
// This catches drift while the watcher was down. Files are enqueued with a
// staggered deadline so the indexer isn't fork-bombed at boot.
func (w *watcher) startupRescan() int {
	queued := 0
	stagger := 0
	for _, r := range w.cfg.Repos {
		_ = filepath.WalkDir(r.Path, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(r.Path, p)
			if d.IsDir() {
				if rel != "." && matchesIgnore(rel, w.cfg.Ignore) {
					return filepath.SkipDir
				}
				return nil
			}
			if matchesIgnore(rel, w.cfg.Ignore) {
				return nil
			}
			if !w.trackedExt(r.Repo, rel) {
				return nil
			}
			// Stagger startup work so we don't queue 10k files all due at once.
			key := r.Repo + "\x00" + rel
			w.mu.Lock()
			w.pending[key] = &pendingChange{
				repo:     r.Repo,
				root:     r.Path,
				rel:      rel,
				deadline: time.Now().Add(time.Duration(w.cfg.DebounceMs+stagger) * time.Millisecond),
			}
			w.mu.Unlock()
			stagger += 20 // 50 files/sec target throughput after first batch
			queued++
			return nil
		})
	}
	w.mu.Lock()
	w.metrics.pendingDepth.Store(int64(len(w.pending)))
	w.mu.Unlock()
	return queued
}

// startMetricsServer exposes /healthz with watcher liveness + counters.
func (w *watcher) startMetricsServer(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		last := w.metrics.lastEventAt.Load()
		var lastISO string
		if last > 0 {
			lastISO = time.Unix(0, last).UTC().Format(time.RFC3339Nano)
		}
		body := map[string]any{
			"version":                       watcherVersion,
			"status":                        "ok",
			"started_at":                    w.metrics.startedAt.UTC().Format(time.RFC3339),
			"uptime_seconds":                int64(time.Since(w.metrics.startedAt).Seconds()),
			"last_event_at":                 lastISO,
			"events_since_start":            w.metrics.eventsSinceStart.Load(),
			"successful_upserts_since_start": w.metrics.successfulUpsertsSince.Load(),
			"failed_upserts_since_start":    w.metrics.failedUpsertsSinceStart.Load(),
			"watched_paths_count":           w.metrics.watchedPathsCount.Load(),
			"pending_depth":                 w.metrics.pendingDepth.Load(),
			"repos":                         len(w.cfg.Repos),
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(body)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Second}
	go func() {
		log.Printf("metrics: /healthz on http://%s/healthz", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server: %v", err)
		}
	}()
}

func main() {
	cfgPath := flag.String("config", os.Getenv("HOME")+"/.fabric/watcher.yaml", "path to watcher.yaml")
	verbose := flag.Bool("verbose", false, "log every fsnotify event")
	noRescan := flag.Bool("no-rescan", false, "skip the startup full rescan")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.FabricKey == "" {
		log.Fatal("fabric_key (or FABRIC_KEY env) required")
	}
	doRescan := true
	if cfg.StartupRescan != nil {
		doRescan = *cfg.StartupRescan
	}
	if *noRescan {
		doRescan = false
	}

	fsn, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("fsnotify: %v", err)
	}
	defer fsn.Close()

	w := &watcher{
		cfg:     cfg,
		verbose: *verbose,
		pending: map[string]*pendingChange{},
		fsn:     fsn,
		http:    &http.Client{Timeout: 15 * time.Second},
		repos:   map[string]*repoCfg{},
		exts:    buildExtMap(cfg.Repos),
	}
	w.metrics.startedAt = time.Now()

	for i := range cfg.Repos {
		r := &cfg.Repos[i]
		abs, err := filepath.Abs(r.Path)
		if err != nil {
			log.Fatalf("abs %s: %v", r.Path, err)
		}
		r.Path = abs
		w.repos[abs] = r
		if err := w.addRecursive(abs); err != nil {
			log.Fatalf("watch %s: %v", abs, err)
		}
		log.Printf("watching %s (%s) → langs=%v", r.Repo, abs, r.Languages)
	}

	w.startMetricsServer(cfg.MetricsAddr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(time.Duration(cfg.DebounceMs/2+50) * time.Millisecond)
	defer tick.Stop()

	log.Printf("fabric-watcher v%s started (debounce=%dms, fabric=%s, %d repos, metrics=%s)",
		watcherVersion, cfg.DebounceMs, cfg.FabricURL, len(cfg.Repos), cfg.MetricsAddr)

	if doRescan {
		go func() {
			n := w.startupRescan()
			log.Printf("startup rescan: queued %d tracked files for reconciliation", n)
		}()
	}

	for {
		select {
		case <-stop:
			log.Printf("shutdown: flushing pending changes…")
			n := w.drainAll(ctx)
			log.Printf("shutdown: flushed %d", n)
			return

		case <-tick.C:
			w.drainOnce(ctx)

		case ev, ok := <-w.fsn.Events:
			if !ok {
				return
			}
			if *verbose {
				log.Printf("ev %s %s", ev.Op, ev.Name)
			}
			w.metrics.eventsSinceStart.Add(1)
			w.metrics.lastEventAt.Store(time.Now().UnixNano())
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = w.addRecursive(ev.Name)
					continue
				}
			}
			repo, rel := w.resolveRepo(ev.Name)
			if repo == nil {
				continue
			}
			if matchesIgnore(rel, cfg.Ignore) {
				continue
			}
			if !w.trackedExt(repo.Repo, rel) {
				continue
			}
			deleted := ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0
			w.enqueue(repo.Repo, repo.Path, rel, deleted)

		case err, ok := <-w.fsn.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %v", err)
		}
	}
}

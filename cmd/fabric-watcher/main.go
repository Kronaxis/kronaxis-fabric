// fabric-watcher: inotify-driven incremental code-graph reindex.
//
// Watches each configured repo recursively. When a tracked source file is
// written/created/removed, after a per-file debounce window we:
//
//   1. POST /v1/symbol/reindex   (soft-delete previous symbols for that file)
//   2. Re-invoke the indexer with --file <rel> --root <repo path>
//      so symbols are repopulated. Removals stop at step 1.
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
	FabricURL   string    `yaml:"fabric_url"`
	FabricKey   string    `yaml:"fabric_key"`
	IndexerPath string    `yaml:"indexer_path"`
	Repos       []repoCfg `yaml:"repos"`
	DebounceMs  int       `yaml:"debounce_ms"`
	Ignore      []string  `yaml:"ignore"`
}

type pendingChange struct {
	repo     string
	root     string
	rel      string
	deleted  bool
	deadline time.Time
}

type watcher struct {
	cfg     *config
	verbose bool

	mu      sync.Mutex
	pending map[string]*pendingChange // key: repo + "\x00" + rel

	fsn   *fsnotify.Watcher
	http  *http.Client
	repos map[string]*repoCfg        // path → repo
	exts  map[string]map[string]bool // repo → set of extensions
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
		} else if w.verbose {
			log.Printf("watch + %s", p)
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
	w.pending[key] = &pendingChange{
		repo:     repo,
		root:     root,
		rel:      rel,
		deleted:  deleted,
		deadline: time.Now().Add(time.Duration(w.cfg.DebounceMs) * time.Millisecond),
	}
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
	w.mu.Unlock()
	for _, p := range ready {
		w.process(ctx, p)
	}
	return len(ready)
}

func (w *watcher) process(ctx context.Context, p *pendingChange) {
	if err := w.postReindex(ctx, p.repo, p.rel); err != nil {
		log.Printf("reindex %s:%s warn: %v", p.repo, p.rel, err)
	} else if w.verbose {
		log.Printf("reindex %s:%s cleared", p.repo, p.rel)
	}
	if p.deleted {
		log.Printf("removed %s:%s", p.repo, p.rel)
		return
	}
	if err := w.invokeIndexer(ctx, p.repo, p.root, p.rel); err != nil {
		log.Printf("indexer %s:%s warn: %v", p.repo, p.rel, err)
		return
	}
	log.Printf("updated %s:%s", p.repo, p.rel)
}

func (w *watcher) postReindex(ctx context.Context, repo, rel string) error {
	body, _ := json.Marshal(map[string]string{"repo": repo, "file_path": rel})
	req, err := http.NewRequestWithContext(ctx, "POST", w.cfg.FabricURL+"/v1/symbol/reindex", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.FabricKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
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

func main() {
	cfgPath := flag.String("config", os.Getenv("HOME")+"/.fabric/watcher.yaml", "path to watcher.yaml")
	verbose := flag.Bool("verbose", false, "log every fsnotify event")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.FabricKey == "" {
		log.Fatal("fabric_key (or FABRIC_KEY env) required")
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(time.Duration(cfg.DebounceMs/2+50) * time.Millisecond)
	defer tick.Stop()

	log.Printf("fabric-watcher v%s started (debounce=%dms, fabric=%s, %d repos)",
		watcherVersion, cfg.DebounceMs, cfg.FabricURL, len(cfg.Repos))

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

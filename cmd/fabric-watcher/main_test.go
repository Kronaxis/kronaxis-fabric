package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- ignore-pattern matcher ----------

func TestMatchesIgnore_Directory(t *testing.T) {
	patterns := []string{".git/", "node_modules/", "vendor/", "__pycache__/"}
	cases := map[string]bool{
		".git/config":                 true,
		"src/.git/HEAD":               true,
		"node_modules/foo/index.js":   true,
		"app/vendor/bundle/x.rb":      true,
		"src/__pycache__/foo.cpython": true,
		"node_modules":                true, // bare segment is itself a match — addRecursive skips this dir
		"src/main.go":                 false,
		"docs/git-guide.md":           false, // basename ≠ ".git"
		"vendored/lib.go":             false, // segment ≠ "vendor"
	}
	for input, want := range cases {
		// Normalise to OS path separator.
		input = filepath.FromSlash(input)
		got := matchesIgnore(input, patterns)
		if got != want {
			t.Errorf("matchesIgnore(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMatchesIgnore_Glob(t *testing.T) {
	patterns := []string{"*.pyc", "*.tmp"}
	cases := map[string]bool{
		"src/foo.pyc":       true,
		"src/a/b/baz.tmp":   true,
		"src/foo.py":        false,
		"src/pyc.go":        false, // extension matters
	}
	for input, want := range cases {
		input = filepath.FromSlash(input)
		got := matchesIgnore(input, patterns)
		if got != want {
			t.Errorf("matchesIgnore(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestMatchesIgnore_EmptyPatterns(t *testing.T) {
	if matchesIgnore("src/main.go", []string{"", ""}) {
		t.Errorf("empty patterns must not match anything")
	}
}

// ---------- debouncer ----------

// newTestWatcher returns a watcher with no fsnotify or HTTP client; used to
// drive enqueue/drain in isolation.
func newTestWatcher(t *testing.T, debounceMs int) *watcher {
	t.Helper()
	return &watcher{
		cfg: &config{
			DebounceMs:   debounceMs,
			BackoffMaxMs: 60_000,
			IndexerPath:  "/bin/true",
		},
		pending: map[string]*pendingChange{},
		http:    &http.Client{Timeout: time.Second},
	}
}

func TestDebounce_CoalescesRapidEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	w := newTestWatcher(t, 50)
	w.cfg.FabricURL = srv.URL
	w.cfg.FabricKey = "test"
	w.metrics.startedAt = time.Now()
	// Use deleted=true so process() doesn't try to fork the python indexer.
	for i := 0; i < 10; i++ {
		w.enqueue("repo", "/root", "src/foo.go", true)
	}
	if len(w.pending) != 1 {
		t.Fatalf("expected 1 pending entry after 10 enqueues on same file, got %d", len(w.pending))
	}
	// Not ready yet.
	ready := w.drainOnce(context.Background())
	if ready != 0 {
		t.Fatalf("expected 0 ready before debounce window, got %d", ready)
	}
	// Wait past debounce window.
	time.Sleep(80 * time.Millisecond)
	ready = w.drainOnce(context.Background())
	if ready != 1 {
		t.Fatalf("expected 1 ready after debounce window, got %d", ready)
	}
	if len(w.pending) != 0 {
		t.Fatalf("expected pending empty after drain, got %d", len(w.pending))
	}
}

func TestDebounce_KeepsDistinctFilesSeparate(t *testing.T) {
	w := newTestWatcher(t, 50)
	w.enqueue("repo", "/root", "src/a.go", false)
	w.enqueue("repo", "/root", "src/b.go", false)
	w.enqueue("repo", "/root", "src/c.go", false)
	if len(w.pending) != 3 {
		t.Fatalf("expected 3 distinct pending entries, got %d", len(w.pending))
	}
}

func TestDebounce_DeleteMarksDeleted(t *testing.T) {
	w := newTestWatcher(t, 50)
	w.enqueue("repo", "/root", "src/foo.go", false)
	w.enqueue("repo", "/root", "src/foo.go", true) // delete supersedes
	got := w.pending["repo\x00src/foo.go"]
	if got == nil {
		t.Fatal("expected entry for src/foo.go")
	}
	if !got.deleted {
		t.Fatalf("delete event should mark entry as deleted")
	}
}

// ---------- backoff requeue ----------

func TestRequeueWithBackoff_GrowsDelay(t *testing.T) {
	w := newTestWatcher(t, 50)
	w.cfg.BackoffMaxMs = 60_000
	p := &pendingChange{repo: "r", root: "/root", rel: "src/x.go"}
	start := time.Now()
	w.requeueWithBackoff(p)
	if p.attempts != 1 {
		t.Fatalf("expected attempts=1, got %d", p.attempts)
	}
	want1 := 1000 * time.Millisecond
	if got := p.deadline.Sub(start); got < want1-50*time.Millisecond || got > want1+200*time.Millisecond {
		t.Errorf("attempt 1 deadline = %v, want ~1s", got)
	}
	// Remove so next requeue lands cleanly.
	delete(w.pending, "r\x00src/x.go")
	start = time.Now()
	p.attempts = 1
	w.requeueWithBackoff(p)
	want2 := 2000 * time.Millisecond
	if got := p.deadline.Sub(start); got < want2-50*time.Millisecond || got > want2+200*time.Millisecond {
		t.Errorf("attempt 2 deadline = %v, want ~2s", got)
	}
}

func TestRequeueWithBackoff_Caps(t *testing.T) {
	w := newTestWatcher(t, 50)
	w.cfg.BackoffMaxMs = 5000
	p := &pendingChange{repo: "r", root: "/root", rel: "src/x.go", attempts: 20}
	start := time.Now()
	w.requeueWithBackoff(p)
	// 1000 << 20 would overflow / huge; must be capped at 5000ms.
	if got := p.deadline.Sub(start); got > 6*time.Second {
		t.Errorf("backoff not capped: deadline = %v", got)
	}
}

// ---------- 5xx requeue end-to-end ----------

func TestProcess_5xx_RequeuesNoDrop(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(rw, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := newTestWatcher(t, 10)
	w.cfg.FabricURL = srv.URL
	w.cfg.FabricKey = "test"
	w.metrics.startedAt = time.Now()

	// Enqueue one change, drive process() directly.
	p := &pendingChange{repo: "r", root: "/root", rel: "src/x.go", deleted: true}
	w.process(context.Background(), p)
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 POST attempt, got %d", attempts.Load())
	}
	w.mu.Lock()
	depth := len(w.pending)
	w.mu.Unlock()
	if depth != 1 {
		t.Fatalf("5xx response should requeue (no drop); pending depth = %d", depth)
	}
	if w.metrics.failedUpsertsSinceStart.Load() != 1 {
		t.Fatalf("failed_upserts counter not incremented")
	}
}

func TestProcess_4xx_DropsNoRequeue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	w := newTestWatcher(t, 10)
	w.cfg.FabricURL = srv.URL
	w.cfg.FabricKey = "test"
	w.metrics.startedAt = time.Now()

	p := &pendingChange{repo: "r", root: "/root", rel: "src/x.go", deleted: true}
	w.process(context.Background(), p)
	w.mu.Lock()
	depth := len(w.pending)
	w.mu.Unlock()
	if depth != 0 {
		t.Fatalf("4xx response should drop; pending depth = %d", depth)
	}
}

// ---------- /healthz endpoint ----------

func TestHealthz_ServesMetrics(t *testing.T) {
	w := newTestWatcher(t, 50)
	w.metrics.startedAt = time.Now()
	w.metrics.eventsSinceStart.Store(7)
	w.metrics.watchedPathsCount.Store(42)
	w.metrics.lastEventAt.Store(time.Now().UnixNano())

	// Pick a random free port via httptest hijack — easiest: use mux directly.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		last := w.metrics.lastEventAt.Load()
		var lastISO string
		if last > 0 {
			lastISO = time.Unix(0, last).UTC().Format(time.RFC3339Nano)
		}
		body := map[string]any{
			"version":             watcherVersion,
			"status":              "ok",
			"events_since_start":  w.metrics.eventsSinceStart.Load(),
			"watched_paths_count": w.metrics.watchedPathsCount.Load(),
			"last_event_at":       lastISO,
		}
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "ok" {
		t.Errorf("status not ok: %v", out["status"])
	}
	if out["events_since_start"].(float64) != 7 {
		t.Errorf("events_since_start = %v, want 7", out["events_since_start"])
	}
}

// ---------- concurrent enqueue (race detector) ----------

func TestEnqueue_Concurrent(t *testing.T) {
	w := newTestWatcher(t, 50)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				w.enqueue("repo", "/root", "src/a.go", false)
				w.enqueue("repo", "/root", "src/b.go", false)
			}
		}(i)
	}
	wg.Wait()
	if len(w.pending) != 2 {
		t.Fatalf("expected 2 entries after concurrent enqueues, got %d", len(w.pending))
	}
}

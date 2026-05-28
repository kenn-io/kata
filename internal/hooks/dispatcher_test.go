package hooks

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kata/internal/db"
)

// mustNewDispatcher builds a Dispatcher rooted at a fresh temp KataHome with
// no-op resolvers and returns the dispatcher, a buffer capturing the daemon
// log, and the absolute path to the runs.jsonl appender for that DB.
func mustNewDispatcher(t *testing.T, hooks []ResolvedHook, cfg Config) (*Dispatcher, *strings.Builder, string) {
	t.Helper()
	root := t.TempDir()
	const dbHash = "testdbhash01"
	logBuf := &strings.Builder{}
	deps := DispatcherDeps{
		DBHash:          dbHash,
		KataHome:        root,
		DaemonLog:       log.New(logBuf, "", 0),
		AliasResolver:   func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) { return AliasSnapshot{}, false, nil },
		IssueResolver:   func(_ context.Context, _ int64) (IssueSnapshot, error) { return IssueSnapshot{}, nil },
		CommentResolver: func(_ context.Context, _ int64) (CommentSnapshot, error) { return CommentSnapshot{}, nil },
		ProjectResolver: func(_ context.Context, _ int64) (ProjectSnapshot, error) { return ProjectSnapshot{}, nil },
		Now:             time.Now,
		GraceWindow:     50 * time.Millisecond,
	}
	loaded := LoadedConfig{Snapshot: Snapshot{Hooks: hooks}, Config: cfg}
	d, err := New(loaded, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.Shutdown(ctx)
	})
	return d, logBuf, filepath.Join(root, "hooks", dbHash, "runs.jsonl")
}

// newTestHook builds a ResolvedHook that runs the hookprobe binary with the
// given args and a 2s timeout. event "*" matches every event; any other
// value is matched exactly.
func newTestHook(t *testing.T, event string, args ...string) ResolvedHook {
	t.Helper()
	match := matchExact(event)
	if event == "*" {
		match = matchAlways()
	}
	return ResolvedHook{
		Index:      0,
		Event:      event,
		Match:      match,
		Command:    hookprobePath(t),
		Args:       args,
		Timeout:    2 * time.Second,
		WorkingDir: t.TempDir(),
	}
}

// enqueueEvents pushes count events of the given type into d, with sequential
// IDs starting at startID and ProjectID/ProjectName placeholders.
func enqueueEvents(d *Dispatcher, eventType string, startID, count int) {
	for i := 0; i < count; i++ {
		d.Enqueue(db.Event{
			ID:          int64(startID + i),
			Type:        eventType,
			ProjectID:   1,
			ProjectName: "x",
		})
	}
}

func TestDispatcher_NewNoop_ImplementsSink(t *testing.T) {
	s := NewNoop()
	s.Enqueue(db.Event{ID: 1, Type: "issue.created"}) // must not panic
	if _, ok := s.(*Dispatcher); ok {
		t.Fatal("NewNoop should not return *Dispatcher")
	}
}

func TestDispatcher_Enqueue_RoutesToMatchingHooks(t *testing.T) {
	hookA := newTestHook(t, "issue.created", "exit", "0")
	hookB := newTestHook(t, "issue.updated", "exit", "0")
	hookB.Index = 1
	cfg := defaultConfig()
	cfg.PoolSize = 2
	cfg.QueueCap = 8
	d, _, runsPath := mustNewDispatcher(t, []ResolvedHook{hookA, hookB}, cfg)
	enqueueEvents(d, "issue.created", 100, 1)
	if !waitForLines(t, runsPath, 1, 5*time.Second) {
		t.Fatal("expected 1 run for hookA")
	}
	enqueueEvents(d, "issue.updated", 101, 1)
	if !waitForLines(t, runsPath, 2, 5*time.Second) {
		t.Fatal("expected 2 runs after second event")
	}
}

func TestDispatcher_Enqueue_QueueFullDropsAndCounts(t *testing.T) {
	slow := newTestHook(t, "*", "sleep", "200ms")
	cfg := defaultConfig()
	cfg.PoolSize = 1
	cfg.QueueCap = 1
	cfg.QueueFullLogInterval = 10 * time.Millisecond
	d, _, _ := mustNewDispatcher(t, []ResolvedHook{slow}, cfg)
	enqueueEvents(d, "issue.created", 200, 10)
	// At least N-2 should drop (1 in queue + 1 in flight + N-2 dropped).
	if got := d.dropped.Load(); got < 5 {
		t.Fatalf("dropped=%d, want >=5", got)
	}
}

func TestDispatcher_Enqueue_AfterShutdown_NoOp(t *testing.T) {
	cfg := defaultConfig()
	d, _, _ := mustNewDispatcher(t, nil, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	d.Enqueue(db.Event{ID: 1, Type: "issue.created"}) // must not panic
}

func TestDispatcher_Shutdown_Idempotent(t *testing.T) {
	cfg := defaultConfig()
	d, _, _ := mustNewDispatcher(t, nil, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown should return nil, got %v", err)
	}
}

func TestDispatcher_Shutdown_Timeout_ReportsInflight(t *testing.T) {
	stuck := newTestHook(t, "*", "term-ignore", "10s")
	stuck.Timeout = 5 * time.Second
	cfg := defaultConfig()
	cfg.PoolSize = 1
	cfg.QueueCap = 4
	d, logBuf, _ := mustNewDispatcher(t, []ResolvedHook{stuck}, cfg)
	enqueueEvents(d, "issue.created", 300, 1)
	waitForInflight(t, d, 1, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown with 50ms ctx vs term-ignore should return error")
	}
	if !strings.Contains(logBuf.String(), "timed out") {
		t.Fatalf("daemon log missing 'timed out': %q", logBuf.String())
	}
}

func TestDispatcher_Shutdown_DropsQueued(t *testing.T) {
	hold := newTestHook(t, "*", "sleep", "500ms")
	cfg := defaultConfig()
	cfg.PoolSize = 1
	cfg.QueueCap = 4
	d, _, runsPath := mustNewDispatcher(t, []ResolvedHook{hold}, cfg)
	enqueueEvents(d, "issue.created", 400, 5)
	time.Sleep(50 * time.Millisecond) // worker has popped one
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	lines := countJSONLLines(runsPath)
	// Only the in-flight job should have produced a line. (4xx all
	// completing would be > 1.) Allow <=2 to tolerate edge timing.
	if lines > 2 {
		t.Fatalf("expected <=2 runs after shutdown, got %d", lines)
	}
}

func TestDispatcher_Reload_AtomicWithEnqueue(t *testing.T) {
	first := newTestHook(t, "*", "exit", "0")
	cfg := defaultConfig()
	d, _, _ := mustNewDispatcher(t, []ResolvedHook{first}, cfg)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		enqueueEvents(d, "issue.created", 0, 200)
	}()
	for i := 0; i < 50; i++ {
		newHook := newTestHook(t, "issue.created", "exit", "0")
		d.Reload(LoadedConfig{Snapshot: Snapshot{Hooks: []ResolvedHook{newHook}}, Config: cfg})
		time.Sleep(2 * time.Millisecond)
	}
	wg.Wait()
}

func TestDispatcher_AliasResolverContext_CancelsOnShutdown(t *testing.T) {
	h := newTestHook(t, "*", "sleep", "5s")
	h.Timeout = 30 * time.Second
	cfg := defaultConfig()
	cfg.PoolSize = 1
	cfg.QueueCap = 1

	// Capture the ctx the resolver was given.
	var (
		capturedCtx context.Context
		captureMu   sync.Mutex
		captured    sync.Once
	)
	deps := DispatcherDeps{
		DBHash:    "testdbhash01",
		KataHome:  t.TempDir(),
		DaemonLog: log.New(&strings.Builder{}, "", 0),
		AliasResolver: func(ctx context.Context, _ db.Event) (AliasSnapshot, bool, error) {
			captured.Do(func() {
				captureMu.Lock()
				capturedCtx = ctx
				captureMu.Unlock()
			})
			return AliasSnapshot{}, false, nil
		},
		IssueResolver:   func(_ context.Context, _ int64) (IssueSnapshot, error) { return IssueSnapshot{}, nil },
		CommentResolver: func(_ context.Context, _ int64) (CommentSnapshot, error) { return CommentSnapshot{}, nil },
		ProjectResolver: func(_ context.Context, _ int64) (ProjectSnapshot, error) { return ProjectSnapshot{}, nil },
		Now:             time.Now,
		GraceWindow:     50 * time.Millisecond,
	}
	loaded := LoadedConfig{Snapshot: Snapshot{Hooks: []ResolvedHook{h}}, Config: cfg}
	d, err := New(loaded, deps)
	if err != nil {
		t.Fatal(err)
	}

	enqueueEvents(d, "issue.created", 500, 1)
	// Wait briefly for the worker to invoke the resolver.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		captureMu.Lock()
		got := capturedCtx
		captureMu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	captureMu.Lock()
	got := capturedCtx
	captureMu.Unlock()
	if got == nil {
		t.Fatal("alias resolver was never invoked")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Shutdown(ctx)
	select {
	case <-got.Done():
	case <-time.After(time.Second):
		t.Fatal("alias resolver context did not cancel after Shutdown")
	}
}

func waitForLines(t *testing.T, path string, n int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if countJSONLLines(path) >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForInflight(t *testing.T, d *Dispatcher, n int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if d.inflight.Load() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d in-flight hook run(s); got %d", n, d.inflight.Load())
}

// countJSONLLines returns the number of non-empty lines in path. A
// missing file or empty file both count as 0; the appender creates an
// empty runs.jsonl at startup, so naive `Count("\n")+1` would falsely
// report 1 line on every empty file.
func countJSONLLines(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir()
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}

func matchExact(want string) func(string) bool { return func(s string) bool { return s == want } }
func matchAlways() func(string) bool           { return func(string) bool { return true } }

// Sentinel keepalive (matches existing hooks-package test convention)
// so that future tests can reference errors without re-importing.
var _ = errors.New

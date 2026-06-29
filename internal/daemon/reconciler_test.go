package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/embedding"
)

// newReconcilerTestStore opens a real sqlitestore.Store at a temp path and
// registers cleanup. The reconciler exercises live storage rather than a mock
// so the dirty-target predicate and upsert round-trip are covered end to end.
func newReconcilerTestStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// fakeEmbedder implements the embedder interface the reconciler depends on.
type fakeEmbedder struct {
	fp   string
	dims int
	err  error
	n    int
}

func (f *fakeEmbedder) Fingerprint() string { return f.fp }
func (f *fakeEmbedder) Dims() int           { return f.dims }
func (f *fakeEmbedder) BatchSize() int      { return 64 }
func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.n += len(texts)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestReconcileOnceEmbedsDirtyTargets(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2}
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 3 {
		t.Fatalf("embedded %d, want 3", emb.n)
	}
	// Second pass: nothing dirty.
	emb.n = 0
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 0 {
		t.Fatalf("re-embedded %d on clean pass", emb.n)
	}
	if h := r.Health(); h.Backlog != 0 || h.LastError != "" {
		t.Fatalf("unexpected health: %#v", h)
	}
	if h := r.Health(); h.LastSuccessAt == nil {
		t.Fatal("a clean pass must record LastSuccessAt")
	}
}

// TestReconcileOnceClearsBacklogAfterDrain pins the /health freshness signal:
// once a cycle embeds the last dirty batch, Backlog must read 0 immediately,
// not stay at the pre-embed count until the next 5m sweep. A partial batch
// does not self-wake, so a stale gauge here would lie to operators (and to the
// e2e that polls backlog==0) for minutes.
func TestReconcileOnceClearsBacklogAfterDrain(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
		t.Fatal(err)
	}
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2}
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 64})

	// A single cycle that embeds the only dirty issue must leave backlog at 0.
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 1 {
		t.Fatalf("embedded %d, want 1", emb.n)
	}
	if h := r.Health(); h.Backlog != 0 {
		t.Fatalf("backlog after draining the queue = %d, want 0", h.Backlog)
	}
}

// TestReconcileOnceFullBatchKeepsBacklogNonZero is the counterpart to the
// drain test: a FULL batch (more dirty rows remain) must NOT reset the gauge to
// 0. Clearing it would make /health report backlog==0 while the index is still
// incomplete, letting an operator or a polling test proceed too early. With
// BatchSize 1 and 2 dirty issues the cycle embeds one and leaves one, so the
// gauge must read the batch count (1), not 0. The next cycle re-lists and
// reports the true remaining count.
func TestReconcileOnceFullBatchKeepsBacklogNonZero(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 2; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2}
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 1})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 1 {
		t.Fatalf("embedded %d, want 1 (one batch)", emb.n)
	}
	if h := r.Health(); h.Backlog != 1 {
		t.Fatalf("backlog after a full batch = %d, want 1 (queue not yet drained)", h.Backlog)
	}
}

func TestReconcileDefinitiveErrorPinsHealth(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	_, _, _ = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"})
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2, err: &embedding.APIError{StatusCode: 401, Body: "bad key"}}
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 64})

	err := r.reconcileOnce(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *embedding.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want APIError, got %v", err)
	}
	if h := r.Health(); h.LastError == "" {
		t.Fatal("health LastError not set")
	}
}

func TestNextBackoffClassifiesErrors(t *testing.T) {
	r := NewReconciler(newReconcilerTestStore(t), &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2},
		ReconcilerConfig{MinBackoff: time.Second, MaxBackoff: 5 * time.Minute})

	// Definitive 4xx pins straight to the max, regardless of current backoff.
	if got := r.nextBackoff(time.Second, &embedding.APIError{StatusCode: 401}); got != 5*time.Minute {
		t.Fatalf("definitive: got %v, want max 5m", got)
	}
	// 429 with Retry-After honors the server's delay.
	got := r.nextBackoff(time.Second, &embedding.APIError{StatusCode: 429, RetryAfter: 7 * time.Second})
	if got != 7*time.Second {
		t.Fatalf("429 retry-after: got %v, want 7s", got)
	}
	// 429 without Retry-After falls back to exponential doubling.
	if got := r.nextBackoff(2*time.Second, &embedding.APIError{StatusCode: 429}); got != 4*time.Second {
		t.Fatalf("429 no retry-after: got %v, want 4s", got)
	}
	// Transient (non-APIError) errors double, capped at MaxBackoff.
	if got := r.nextBackoff(2*time.Second, errors.New("connection refused")); got != 4*time.Second {
		t.Fatalf("transient: got %v, want 4s", got)
	}
	if got := r.nextBackoff(4*time.Minute, errors.New("connection refused")); got != 5*time.Minute {
		t.Fatalf("transient cap: got %v, want max 5m", got)
	}
}

func TestReconcileOnceFullBatchWakesToContinue(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 5; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2}
	// BatchSize 2 leaves more dirty targets than one batch can clear.
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 2})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 2 {
		t.Fatalf("embedded %d, want 2 (one batch)", emb.n)
	}
	// A full batch must re-arm Wake so Run continues draining the backlog.
	select {
	case <-r.wake:
	default:
		t.Fatal("full batch did not wake the reconciler to continue")
	}
}

func TestReconcileOncePartialBatchDoesNotWake(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	_, _, _ = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"})
	emb := &fakeEmbedder{fp: "a" + repeat63reconciler, dims: 2}
	r := NewReconciler(store, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.wake:
		t.Fatal("partial batch should not re-arm Wake")
	default:
	}
}

// flakyEmbedder fails its first failUntil Embed calls with err, then succeeds.
// It records the total number of successfully embedded texts. All access is
// guarded so Run's goroutine and the test goroutine can read/write safely.
type flakyEmbedder struct {
	fp        string
	dims      int
	err       error
	failUntil int

	mu       sync.Mutex
	calls    int
	embedded int
}

func (f *flakyEmbedder) Fingerprint() string { return f.fp }
func (f *flakyEmbedder) Dims() int           { return f.dims }
func (f *flakyEmbedder) BatchSize() int      { return 64 }
func (f *flakyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failUntil {
		return nil, f.err
	}
	f.embedded += len(texts)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (f *flakyEmbedder) embeddedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.embedded
}

func (f *flakyEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRunDrainsAfterTransientFailureThenExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	// Fail the first Embed (transient), then succeed. With tiny backoffs the
	// retry happens almost immediately, so the loop drains the backlog quickly.
	emb := &flakyEmbedder{fp: "a" + repeat63reconciler, dims: 2, failUntil: 1, err: errors.New("connection refused")}
	r := NewReconciler(store, emb, ReconcilerConfig{
		BatchSize:  64,
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
		SweepEvery: time.Millisecond,
	})

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	r.Wake()

	// Poll until all three targets are embedded and the backoff has reset on
	// success (LastError cleared, LastSuccessAt set). Poll rather than sleep so
	// the test is robust against scheduling jitter.
	require.Eventually(t, func() bool {
		if emb.embeddedCount() < 3 {
			return false
		}
		h := r.Health()
		return h.LastSuccessAt != nil && h.LastError == "" && h.Backlog == 0
	}, 2*time.Second, time.Millisecond, "reconciler did not drain the backlog after a transient failure")

	// Cancelling ctx must make Run return promptly with the ctx error.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}

	// The failed first attempt must have set LastError at the time; the later
	// success cleared it (backoff-reset-on-success), proving recovery.
	if c := emb.callCount(); c < 2 {
		t.Fatalf("expected at least one retry after the transient failure, got %d Embed calls", c)
	}
}

const repeat63reconciler = "000000000000000000000000000000000000000000000000000000000000000"

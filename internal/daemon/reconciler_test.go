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
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
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

// openTestVectorIndex opens a fresh vector.Index at a temp path and registers
// cleanup.
func openTestVectorIndex(t *testing.T) *vector.Index {
	t.Helper()
	idx, err := vector.Open(context.Background(), filepath.Join(t.TempDir(), "vectors.db"))
	if err != nil {
		t.Fatalf("open vector index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// fakeEmbedder implements the embedder interface the reconciler depends on.
// When err is set, EncodeFunc fails every call, or — with failAfter > 0 —
// only calls after the first failAfter, which succeed normally.
type fakeEmbedder struct {
	model     string
	dims      int
	err       error
	failAfter int
	blockAt   int
	blocked   chan struct{}
	release   chan struct{}
	onCall    func(int)
	calls     int
	n         int
}

func (f *fakeEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: f.model, Dimensions: f.dims, Params: map[string]string{"recipe": "2"}}
}
func (f *fakeEmbedder) BatchSize() int { return 64 }
func (f *fakeEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		f.calls++
		if f.onCall != nil {
			f.onCall(f.calls)
		}
		if f.calls == f.blockAt {
			close(f.blocked)
			<-f.release
		}
		if f.err != nil && f.calls > f.failAfter {
			return nil, f.err
		}
		f.n += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			v := make([]float32, f.dims)
			v[0] = 1
			out[i] = v
		}
		return out, nil
	}
}

func TestReconcileOnceEmbedsAndActivates(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t)
	emb := &fakeEmbedder{model: "m1", dims: 4}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 3 {
		t.Fatalf("encoded %d chunks, want 3 (one per short issue)", emb.n)
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil || !ok || key != emb.Generation().Fingerprint() {
		t.Fatalf("generation not activated after drain: %q %v %v", key, ok, err)
	}
	if h := r.Health(); h.Backlog != 0 || h.LastSuccessAt == nil {
		t.Fatalf("health = %+v", h)
	}
}

// TestReconcileOnceColdStartActivatesBeforeFill pins the cold-start contract:
// with no previously active generation (fresh sidecar or first upgrade), the
// new generation is activated immediately after EnsureBuilding — before the
// fill — so search serves partial results during the initial backfill instead
// of the vector leg being unavailable until the whole corpus is embedded. The
// failing embedder interrupts reconcileOnce mid-fill, proving activation did
// not wait for the fill to complete.
func TestReconcileOnceColdStartActivatesBeforeFill(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	emb := &fakeEmbedder{model: "m1", dims: 4, err: errors.New("connection refused")}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err == nil {
		t.Fatal("expected the failing fill to surface an error")
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != emb.Generation().Fingerprint() {
		t.Fatalf("cold start must activate the generation before the fill: key=%q ok=%v", key, ok)
	}
}

// TestReconcileOnceModelChangeKeepsOldActiveDuringBackfill pins the
// counterpart: when a generation IS already active (model change), the new
// one is built in the background and only cut over once its fill completes —
// an interrupted fill leaves the old generation serving.
func TestReconcileOnceModelChangeKeepsOldActiveDuringBackfill(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	emb1 := &fakeEmbedder{model: "m1", dims: 4}
	r1 := NewReconciler(store, idx, emb1, ReconcilerConfig{BatchSize: 64})
	if err := r1.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}

	emb2 := &fakeEmbedder{model: "m2", dims: 4, err: errors.New("connection refused")}
	r2 := NewReconciler(store, idx, emb2, ReconcilerConfig{BatchSize: 64})
	if err := r2.reconcileOnce(ctx); err == nil {
		t.Fatal("expected the failing fill to surface an error")
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != emb1.Generation().Fingerprint() {
		t.Fatalf("old generation must stay active while the new fill is incomplete: key=%q ok=%v", key, ok)
	}
}

func TestReconcileOnceModelChangeCutsOver(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	r1 := NewReconciler(store, idx, &fakeEmbedder{model: "m1", dims: 4}, ReconcilerConfig{BatchSize: 64})
	if err := r1.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	emb2 := &fakeEmbedder{model: "m2", dims: 4}
	r2 := NewReconciler(store, idx, emb2, ReconcilerConfig{BatchSize: 64})
	if err := r2.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	key, ok, _ := idx.ActiveGeneration(ctx)
	if !ok || key != emb2.Generation().Fingerprint() {
		t.Fatalf("active = %q, want new model's generation", key)
	}
}

func TestReconcileErrorReportsPendingBacklog(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 2; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t)
	emb := &fakeEmbedder{model: "m1", dims: 2, err: &embedding.APIError{StatusCode: 500, Body: "down"}}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err == nil {
		t.Fatal("expected fill error")
	}
	if h := r.Health(); h.Backlog != 2 {
		t.Fatalf("backlog after failed fill = %d, want 2 (documents still pending)", h.Backlog)
	}
}

func TestReconcileReportsProgressDuringFill(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 2; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t)
	release := make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	emb := &fakeEmbedder{
		model:   "m1",
		dims:    2,
		blockAt: 2,
		blocked: make(chan struct{}),
		release: release,
	}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 1})

	done := make(chan error, 1)
	go func() { done <- r.reconcileOnce(ctx) }()
	<-emb.blocked

	if h := r.Health(); h.Backlog != 1 || h.Embedded != 1 {
		t.Fatalf("health while second document is encoding = %+v, want backlog 1 and embedded 1", h)
	}
	close(release)
	release = nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestReconcileReportsSmoothedRateAndETA(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t)
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	defer func() {
		if release != nil {
			close(release)
		}
	}()
	emb := &fakeEmbedder{
		model:   "m1",
		dims:    2,
		blockAt: 3,
		blocked: make(chan struct{}),
		release: release,
		onCall: func(call int) {
			if call == 1 {
				now = now.Add(2 * time.Second)
			} else {
				now = now.Add(time.Second)
			}
		},
	}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{
		BatchSize: 1,
		Now:       func() time.Time { return now },
	})

	done := make(chan error, 1)
	go func() { done <- r.reconcileOnce(ctx) }()
	<-emb.blocked

	h := r.Health()
	if h.Embedded != 2 || h.Backlog != 1 {
		t.Fatalf("health counts while third document is encoding = %+v", h)
	}
	if h.RatePerSecond == nil || h.ETASeconds == nil {
		t.Fatalf("health must report rate and ETA after two progress samples: %+v", h)
	}
	if diff := *h.RatePerSecond - 0.65; diff < -0.001 || diff > 0.001 {
		t.Fatalf("smoothed rate = %f, want 0.65 docs/s", *h.RatePerSecond)
	}
	if *h.ETASeconds != 2 {
		t.Fatalf("ETA = %d seconds, want 2", *h.ETASeconds)
	}
	if h.StartedAt == nil || h.LastProgressAt == nil {
		t.Fatalf("health must expose progress timestamps: %+v", h)
	}

	close(release)
	release = nil
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddingProgressEstimatorResetsAcrossDiscontinuities(t *testing.T) {
	tests := []struct {
		name       string
		generation string
		embedded   int64
		backlog    int64
	}{
		{name: "generation change", generation: "m2", embedded: 2, backlog: 1},
		{name: "total change", generation: "m1", embedded: 2, backlog: 2},
		{name: "progress moved backward", generation: "m1", embedded: 1, backlog: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
			r := NewReconciler(nil, nil, nil, ReconcilerConfig{Now: func() time.Time { return now }})
			r.setCoverage("m1", 0, 0, 3)
			now = now.Add(time.Second)
			r.markDocumentFilled(true)
			now = now.Add(time.Second)
			r.markDocumentFilled(true)
			if h := r.Health(); h.RatePerSecond == nil || h.ETASeconds == nil {
				t.Fatalf("expected established estimate before reset: %+v", h)
			}

			r.setCoverage(tt.generation, tt.embedded, 0, tt.backlog)
			h := r.Health()
			if h.RatePerSecond != nil || h.ETASeconds != nil || h.LastProgressAt != nil {
				t.Fatalf("progress estimate survived discontinuity: %+v", h)
			}
			if h.StartedAt == nil {
				t.Fatalf("reset backfill must enter estimating state: %+v", h)
			}
		})
	}
}

// TestReconcileFillErrorRefreshesPartialBacklog pins the on-error backlog
// refresh: when the fill stamps some documents before failing, health must
// report only what is still pending, not the pre-fill count.
func TestReconcileFillErrorRefreshesPartialBacklog(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 2; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t)
	// First encode call (first document) succeeds and stamps; the second
	// fails, aborting the fill with one document embedded and one pending.
	emb := &fakeEmbedder{model: "m1", dims: 2, failAfter: 1, err: &embedding.APIError{StatusCode: 500, Body: "down"}}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err == nil {
		t.Fatal("expected fill error")
	}
	if emb.n != 1 {
		t.Fatalf("embedded %d documents before the failure, want 1", emb.n)
	}
	if h := r.Health(); h.Backlog != 1 {
		t.Fatalf("backlog after partial fill = %d, want 1 (only the unembedded document)", h.Backlog)
	}
}

func TestReconcileDefinitiveErrorPinsHealth(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	_, _, _ = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"})
	idx := openTestVectorIndex(t)
	emb := &fakeEmbedder{model: "m1", dims: 2, err: &embedding.APIError{StatusCode: 401, Body: "bad key"}}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

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
	r := NewReconciler(newReconcilerTestStore(t), openTestVectorIndex(t), &fakeEmbedder{model: "m1", dims: 2},
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

// flakyEmbedder fails its first failUntil EncodeFunc calls with err, then
// succeeds. It records the total number of successfully encoded texts. All
// access is guarded so Run's goroutine and the test goroutine can read/write
// safely.
type flakyEmbedder struct {
	model     string
	dims      int
	err       error
	failUntil int

	mu       sync.Mutex
	calls    int
	embedded int
}

func (f *flakyEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: f.model, Dimensions: f.dims, Params: map[string]string{"recipe": "2"}}
}
func (f *flakyEmbedder) BatchSize() int { return 64 }
func (f *flakyEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.calls <= f.failUntil {
			return nil, f.err
		}
		f.embedded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			v := make([]float32, f.dims)
			v[0] = 1
			out[i] = v
		}
		return out, nil
	}
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
	// Fail the first EncodeFunc call (transient), then succeed. With tiny
	// backoffs the retry happens almost immediately, so the loop drains the
	// backlog quickly.
	idx := openTestVectorIndex(t)
	emb := &flakyEmbedder{model: "m1", dims: 2, failUntil: 1, err: errors.New("connection refused")}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{
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
		t.Fatalf("expected at least one retry after the transient failure, got %d EncodeFunc calls", c)
	}
}

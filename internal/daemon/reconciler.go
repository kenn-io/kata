package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

// embedder is the subset of *embedding.Client the reconciler needs (an
// interface so tests can substitute a fake).
type embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Fingerprint() string
	Dims() int
	BatchSize() int
}

// ReconcilerConfig tunes the reconciler.
type ReconcilerConfig struct {
	BatchSize  int
	SweepEvery time.Duration // periodic safety sweep; default 5m
	MinBackoff time.Duration // default 1s
	MaxBackoff time.Duration // default 5m
}

// ReconcilerHealth is the operator-visible state surfaced in /health.
type ReconcilerHealth struct {
	Configured      bool       `json:"configured"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastError       string     `json:"-"`
	LastErrorStatus int        `json:"last_error_status,omitempty"`
	Backlog         int64      `json:"backlog"`
}

// Reconciler keeps issue_embeddings fresh by embedding dirty issues.
type Reconciler struct {
	store db.Storage
	emb   embedder
	cfg   ReconcilerConfig
	wake  chan struct{}

	mu     sync.Mutex
	health ReconcilerHealth
}

// NewReconciler constructs a reconciler. It does no I/O.
func NewReconciler(store db.Storage, emb embedder, cfg ReconcilerConfig) *Reconciler {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = emb.BatchSize()
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = 5 * time.Minute
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	return &Reconciler{
		store:  store,
		emb:    emb,
		cfg:    cfg,
		wake:   make(chan struct{}, 1),
		health: ReconcilerHealth{Configured: true},
	}
}

// Wake nudges the reconciler to run a cycle soon (non-blocking, coalesced).
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Health returns a snapshot of reconciler state. markSuccess swaps in a fresh
// *time.Time rather than mutating in place, so today there is no shared pointer
// to race on. Deep-copying LastSuccessAt here is defensive isolation: it locks
// down the "a returned snapshot is never aliased to live state" invariant so a
// future in-place mutation can't silently start leaking through callers.
func (r *Reconciler) Health() ReconcilerHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health
	if r.health.LastSuccessAt != nil {
		t := *r.health.LastSuccessAt
		h.LastSuccessAt = &t
	}
	return h
}

// Run drains dirty work until ctx is cancelled, waking on Wake(), a periodic
// safety sweep, and after backoff on failure.
func (r *Reconciler) Run(ctx context.Context) error {
	backoff := r.cfg.MinBackoff
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		case <-timer.C:
		}
		if err := r.reconcileOnce(ctx); err == nil {
			backoff = r.cfg.MinBackoff
			timer.Reset(r.cfg.SweepEvery)
		} else {
			backoff = r.nextBackoff(backoff, err)
			timer.Reset(backoff)
		}
	}
}

func (r *Reconciler) nextBackoff(cur time.Duration, err error) time.Duration {
	var apiErr *embedding.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Definitive() {
			return r.cfg.MaxBackoff
		}
		if apiErr.RetryAfter > 0 {
			return apiErr.RetryAfter
		}
	}
	next := cur * 2
	if next > r.cfg.MaxBackoff {
		next = r.cfg.MaxBackoff
	}
	return next
}

// reconcileOnce embeds one batch of dirty targets and updates health.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	fp := r.emb.Fingerprint()
	targets, err := r.store.ListEmbedTargets(ctx, fp, r.cfg.BatchSize)
	if err != nil {
		return err
	}
	r.setBacklog(int64(len(targets)))
	if len(targets) == 0 {
		r.markSuccess()
		return nil
	}
	texts := make([]string, len(targets))
	for i, t := range targets {
		texts[i] = embedding.EmbedText(t.Title, t.Body)
	}
	vecs, err := r.emb.Embed(ctx, texts)
	if err != nil {
		r.markError(err)
		return err
	}
	for i, t := range targets {
		if err := r.store.UpsertIssueEmbedding(ctx, db.IssueEmbedding{
			IssueID:                 t.IssueID,
			EmbeddedContentRevision: t.ContentRevision,
			Fingerprint:             fp,
			Dims:                    r.emb.Dims(),
			Vector:                  vecs[i],
		}); err != nil {
			r.markError(err)
			return err
		}
	}
	r.markSuccess()
	// Only a partial batch proves the queue is fully drained, so clear the
	// gauge to 0 then. A full batch may have more dirty rows behind it; leave
	// the gauge at the batch count set above (clearing it would make /health
	// report backlog=0 while the index is still incomplete) and self-wake so
	// the next cycle's ListEmbedTargets recomputes the true remaining count.
	if len(targets) < r.cfg.BatchSize {
		r.setBacklog(0)
	} else {
		r.Wake()
	}
	return nil
}

func (r *Reconciler) markSuccess() {
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.LastSuccessAt = &now
	r.health.LastError = ""
	r.health.LastErrorStatus = 0
}

func (r *Reconciler) markError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.LastError = err.Error()
	var apiErr *embedding.APIError
	if errors.As(err, &apiErr) {
		r.health.LastErrorStatus = apiErr.StatusCode
	} else {
		r.health.LastErrorStatus = 0
	}
}

func (r *Reconciler) setBacklog(n int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.Backlog = n
}

package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// embedder is the subset of *embedding.Client the reconciler needs (an
// interface so tests can substitute a fake).
type embedder interface {
	EncodeFunc() kitvec.EncodeFunc
	Generation() kitvec.Generation
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

// Reconciler keeps the vector sidecar's active generation fresh: it mirrors
// live issue content, fills the desired embedding generation via kit, and
// cuts over to it once fully populated.
type Reconciler struct {
	store db.Storage
	idx   *vector.Index
	emb   embedder
	cfg   ReconcilerConfig
	wake  chan struct{}

	mu     sync.Mutex
	health ReconcilerHealth
}

// NewReconciler constructs a reconciler. It does no I/O.
func NewReconciler(store db.Storage, idx *vector.Index, emb embedder, cfg ReconcilerConfig) *Reconciler {
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
		idx:    idx,
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

// reconcileOnce refreshes the mirror, drains the fill for the desired
// generation, cuts over when the fill completes, and updates health. Fill
// loops internally until no documents are pending, so a successful return
// means the desired generation is fully populated and active.
//
// Cold start (no active generation: fresh sidecar or first upgrade) cuts the
// new generation over immediately, before the fill, so search serves partial
// results during the initial backfill and the health backlog explains the
// coverage. A model change (an active generation exists) keeps the
// build-then-cutover path: the old generation stays active until the new one
// is fully filled.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	if _, err := r.idx.RefreshMirror(ctx, r.store); err != nil {
		r.markError(err)
		return err
	}
	gen := r.emb.Generation()
	key := gen.Fingerprint()
	if err := r.idx.EnsureBuilding(ctx, key, gen); err != nil {
		r.markError(err)
		return err
	}
	if _, active, err := r.idx.ActiveGeneration(ctx); err != nil {
		r.markError(err)
		return err
	} else if !active {
		if err := r.idx.CutOver(ctx, key); err != nil {
			r.markError(err)
			return err
		}
	}
	// Publish the pending count before the fill so /health reports the real
	// backlog during a long backfill instead of the previous cycle's value.
	backlog, err := r.idx.Backlog(ctx, key)
	if err != nil {
		r.markError(err)
		return err
	}
	r.setBacklog(backlog)
	if _, err := r.idx.Fill(ctx, key, r.emb.EncodeFunc(), r.cfg.BatchSize, r.emb.BatchSize()); err != nil {
		if backlog, berr := r.idx.Backlog(ctx, key); berr == nil {
			r.setBacklog(backlog)
		}
		r.markError(err)
		return err
	}
	if err := r.idx.CutOver(ctx, key); err != nil {
		r.markError(err)
		return err
	}
	backlog, err = r.idx.Backlog(ctx, key)
	if err != nil {
		r.markError(err)
		return err
	}
	r.setBacklog(backlog)
	r.markSuccess()
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

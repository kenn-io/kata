package daemon

import (
	"context"
	"errors"
	"math"
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
	Now        func() time.Time
}

// ReconcilerHealth is the operator-visible state surfaced in /health.
type ReconcilerHealth struct {
	Configured      bool       `json:"configured"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastError       string     `json:"-"`
	LastErrorStatus int        `json:"last_error_status,omitempty"`
	Embedded        int64      `json:"embedded"`
	Skipped         int64      `json:"skipped"`
	Backlog         int64      `json:"backlog"`
	RatePerSecond   *float64   `json:"rate_per_second,omitempty"`
	ETASeconds      *int64     `json:"eta_seconds,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	LastProgressAt  *time.Time `json:"last_progress_at,omitempty"`
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
	now   func() time.Time

	mu       sync.Mutex
	health   ReconcilerHealth
	progress embeddingProgress
}

type embeddingProgress struct {
	generation    string
	total         int64
	lastProcessed int64
	lastAt        time.Time
	startedAt     time.Time
	rate          float64
	samples       int
}

const progressRateAlpha = 0.3

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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Reconciler{
		store:  store,
		idx:    idx,
		emb:    emb,
		cfg:    cfg,
		wake:   make(chan struct{}, 1),
		now:    cfg.Now,
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
	if r.health.StartedAt != nil {
		t := *r.health.StartedAt
		h.StartedAt = &t
	}
	if r.health.LastProgressAt != nil {
		t := *r.health.LastProgressAt
		h.LastProgressAt = &t
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
	embedded, skipped, backlog, err := r.idx.Coverage(ctx, key)
	if err != nil {
		r.markError(err)
		return err
	}
	r.setCoverage(key, embedded, skipped, backlog)
	if _, err := r.idx.Fill(ctx, key, r.emb.EncodeFunc(), r.cfg.BatchSize, r.emb.BatchSize(), r.markDocumentFilled); err != nil {
		if embedded, skipped, backlog, coverageErr := r.idx.Coverage(ctx, key); coverageErr == nil {
			r.setCoverage(key, embedded, skipped, backlog)
		}
		r.markError(err)
		return err
	}
	if err := r.idx.CutOver(ctx, key); err != nil {
		r.markError(err)
		return err
	}
	embedded, skipped, backlog, err = r.idx.Coverage(ctx, key)
	if err != nil {
		r.markError(err)
		return err
	}
	r.setCoverage(key, embedded, skipped, backlog)
	r.markSuccess()
	return nil
}

func (r *Reconciler) markDocumentFilled(embedded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if embedded {
		r.health.Embedded++
	} else {
		r.health.Skipped++
	}
	r.health.Backlog--
	r.recordProgressLocked(r.health.Embedded+r.health.Skipped, r.now())
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
	r.health.ETASeconds = nil
	var apiErr *embedding.APIError
	if errors.As(err, &apiErr) {
		r.health.LastErrorStatus = apiErr.StatusCode
	} else {
		r.health.LastErrorStatus = 0
	}
}

func (r *Reconciler) setCoverage(generation string, embedded, skipped, backlog int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health.Embedded = embedded
	r.health.Skipped = skipped
	r.health.Backlog = backlog
	if backlog == 0 {
		r.resetProgressLocked()
		return
	}
	processed := embedded + skipped
	total := processed + backlog
	if r.progress.generation != generation || r.progress.total != total ||
		processed < r.progress.lastProcessed || r.progress.startedAt.IsZero() {
		now := r.now()
		r.progress = embeddingProgress{
			generation:    generation,
			total:         total,
			lastProcessed: processed,
			lastAt:        now,
			startedAt:     now,
		}
		r.health.StartedAt = &now
		r.health.LastProgressAt = nil
		r.health.RatePerSecond = nil
		r.health.ETASeconds = nil
		return
	}
	if processed > r.progress.lastProcessed {
		r.recordProgressLocked(processed, r.now())
	}
}

func (r *Reconciler) recordProgressLocked(processed int64, now time.Time) {
	delta := processed - r.progress.lastProcessed
	elapsed := now.Sub(r.progress.lastAt).Seconds()
	r.progress.lastProcessed = processed
	r.progress.lastAt = now
	r.health.LastProgressAt = &now
	if delta <= 0 || elapsed <= 0 {
		return
	}
	instantRate := float64(delta) / elapsed
	if r.progress.samples == 0 {
		r.progress.rate = instantRate
	} else {
		r.progress.rate = progressRateAlpha*instantRate + (1-progressRateAlpha)*r.progress.rate
	}
	r.progress.samples++
	if r.progress.samples < 2 {
		return
	}
	rate := r.progress.rate
	r.health.RatePerSecond = &rate
	if r.health.Backlog > 0 {
		eta := int64(math.Ceil(float64(r.health.Backlog) / rate))
		r.health.ETASeconds = &eta
	} else {
		r.health.ETASeconds = nil
	}
}

func (r *Reconciler) resetProgressLocked() {
	r.progress = embeddingProgress{}
	r.health.RatePerSecond = nil
	r.health.ETASeconds = nil
	r.health.StartedAt = nil
	r.health.LastProgressAt = nil
}

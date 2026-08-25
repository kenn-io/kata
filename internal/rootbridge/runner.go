package rootbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/kata/internal/db"
)

const (
	defaultExternalRootScanInterval = 30 * time.Second
	defaultExternalRootBatchLimit   = 100
	defaultExternalRootConcurrency  = 4
	maxExternalRootRetryDelay       = 30 * time.Minute
	externalRootWakeBuffer          = 100
)

// RetryJitter returns an adjustment to the exponential retry delay.
// ExternalRootRetryDelay clamps it to ten percent and caps the final delay at 30m.
type RetryJitter func(bindingID int64, failures int, base time.Duration) time.Duration

// Runner polls and wakes external-root bindings with bounded concurrency.
type Runner struct {
	Store         db.Storage
	Reconciler    *Reconciler
	Interval      time.Duration
	WakeCh        chan int64
	MaxConcurrent int
	Now           func() time.Time
	EventSink     func(db.Event)
	ErrorSink     func(error)

	reconcileFn func(context.Context, int64) (RunResult, error)

	once    sync.Once
	mu      sync.Mutex
	pending map[int64]struct{}
}

func (r *Runner) initialize() {
	r.once.Do(func() {
		if r.WakeCh == nil {
			r.WakeCh = make(chan int64, externalRootWakeBuffer)
		}
		r.pending = make(map[int64]struct{})
	})
}

// Wake schedules one binding without blocking its producer. Duplicate queued
// or in-flight work coalesces; a saturated queue drops the wake and polling
// remains the correctness path.
func (r *Runner) Wake(bindingID int64) {
	if r == nil || bindingID <= 0 {
		return
	}
	r.initialize()
	r.mu.Lock()
	if _, exists := r.pending[bindingID]; exists {
		r.mu.Unlock()
		return
	}
	r.pending[bindingID] = struct{}{}
	r.mu.Unlock()
	select {
	case r.WakeCh <- bindingID:
	default:
		r.finished(bindingID)
	}
}

// Run processes wakes and periodic due scans until ctx ends.
func (r *Runner) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("external root runner is unavailable")
	}
	r.initialize()
	interval := r.Interval
	if interval <= 0 {
		interval = defaultExternalRootScanInterval
	}
	concurrency := r.MaxConcurrent
	if concurrency <= 0 || concurrency > defaultExternalRootConcurrency {
		concurrency = defaultExternalRootConcurrency
	}
	if r.reconcileFn == nil && (r.Store == nil || r.Reconciler == nil) {
		return errors.New("external root runner dependencies are unavailable")
	}
	sem := make(chan struct{}, concurrency)
	var workers sync.WaitGroup
	launch := func(bindingID int64) {
		select {
		case sem <- struct{}{}:
			workers.Go(func() {
				defer func() {
					if recover() != nil {
						r.reportReconcileError(ctx, bindingID, errors.New("external root reconciliation panicked"))
					}
					<-sem
					r.finished(bindingID)
				}()
				result, deliveredInline, err := r.reconcile(ctx, bindingID)
				if !deliveredInline && r.EventSink != nil {
					for _, event := range result.Events {
						r.EventSink(event)
					}
				}
				r.reportReconcileError(ctx, bindingID, err)
			})
		case <-ctx.Done():
			r.finished(bindingID)
		}
	}
	scan := func() error {
		if r.Store == nil {
			return nil
		}
		now := time.Now().UTC()
		if r.Now != nil {
			now = r.Now().UTC()
		}
		staleAfter := defaultExternalRootClaimStaleAfter
		if r.Reconciler != nil {
			staleAfter = r.Reconciler.claimStaleAfter
		}
		bindings, err := r.Store.ListDueExternalRootBindings(ctx, now, now.Add(-staleAfter), defaultExternalRootBatchLimit)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			r.Wake(binding.ID)
		}
		return nil
	}
	reportScanError := func(err error) {
		if ctx.Err() == nil && r.ErrorSink != nil {
			r.ErrorSink(fmt.Errorf("scan due external root bindings: %w", err))
		}
	}
	if err := scan(); err != nil {
		reportScanError(err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case bindingID := <-r.WakeCh:
			launch(bindingID)
		case <-ticker.C:
			if err := scan(); err != nil {
				reportScanError(err)
			}
		}
	}
}

func (r *Runner) reconcile(ctx context.Context, bindingID int64) (RunResult, bool, error) {
	if r.reconcileFn != nil {
		result, err := r.reconcileFn(ctx, bindingID)
		return result, false, err
	}
	result, err := r.Reconciler.Run(ctx, bindingID, RunOptions{EventSink: r.EventSink})
	return result, true, err
}

func (r *Runner) reportReconcileError(ctx context.Context, bindingID int64, err error) {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || r.ErrorSink == nil {
		return
	}
	r.ErrorSink(fmt.Errorf("reconcile external root binding %d: %w", bindingID, err))
}

func (r *Runner) finished(bindingID int64) {
	r.mu.Lock()
	delete(r.pending, bindingID)
	r.mu.Unlock()
}

// ExternalRootRetryDelay returns the capped exponential delay for a binding.
func ExternalRootRetryDelay(bindingID int64, failures int, jitter RetryJitter) time.Duration {
	if failures < 0 {
		failures = 0
	}
	base := 30 * time.Second
	for i := 0; i < failures && base < maxExternalRootRetryDelay; i++ {
		base *= 2
		if base > maxExternalRootRetryDelay {
			base = maxExternalRootRetryDelay
		}
	}
	adjustment := deterministicExternalRootJitter(bindingID, failures, base)
	if jitter != nil {
		adjustment = jitter(bindingID, failures, base)
	}
	bound := base / 10
	if adjustment > bound {
		adjustment = bound
	} else if adjustment < -bound {
		adjustment = -bound
	}
	delay := base + adjustment
	if delay > maxExternalRootRetryDelay {
		return maxExternalRootRetryDelay
	}
	return delay
}

// ExternalRootRetryAt builds the immutable retry-time strategy shared by
// background and manual reconciliations.
func ExternalRootRetryAt(jitter RetryJitter) func(db.ExternalRootBinding, time.Time) time.Time {
	return func(binding db.ExternalRootBinding, now time.Time) time.Time {
		return now.Add(ExternalRootRetryDelay(binding.ID, binding.ConsecutiveFailures, jitter))
	}
}

func deterministicExternalRootJitter(bindingID int64, failures int, base time.Duration) time.Duration {
	percent := (bindingID+int64(failures)*17)%21 - 10
	return time.Duration(int64(base) * percent / 100)
}

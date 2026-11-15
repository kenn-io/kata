package sqlitestore

import (
	"context"
	"errors"
	"time"
)

// federationSlowOptimisticPreparation is how long an invalidated optimistic
// preparation may take before the next attempt acquires the writer first.
// Steady writes that invalidated a slow snapshot will likely invalidate the
// next one too, so a second optimistic attempt would only repeat slow work.
const federationSlowOptimisticPreparation = time.Second

// retryFederationPreparation gives optimistic snapshots a bounded number of
// attempts outside the generic elapsed-time budget. Slow preparation can use
// that entire budget before its first BUSY_SNAPSHOT, but must still reach the
// writer-held fallback. Only that fallback uses the normal contention backoff.
func (d *Store) retryFederationPreparation(ctx context.Context, op func(locked bool) error) error {
	for range federationIngestOptimisticAttempts {
		started := time.Now()
		if err := op(false); err == nil || (!IsTransient(err) && !errors.Is(err, errStaleFederationPreparation)) {
			return err
		}
		if time.Since(started) >= federationSlowOptimisticPreparation {
			break
		}
	}
	return d.RetryTransient(ctx, func() error { return op(true) })
}

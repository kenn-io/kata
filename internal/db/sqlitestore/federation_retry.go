package sqlitestore

import "context"

// retryFederationPreparation gives optimistic snapshots a bounded number of
// attempts outside the generic elapsed-time budget. Slow preparation can use
// that entire budget before its first BUSY_SNAPSHOT, but must still reach the
// writer-held fallback. Only that fallback uses the normal contention backoff.
func (d *Store) retryFederationPreparation(ctx context.Context, op func(locked bool) error) error {
	for range federationIngestOptimisticAttempts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := op(false); err == nil || !IsTransient(err) {
			return err
		}
	}
	return d.RetryTransient(ctx, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return op(true)
	})
}

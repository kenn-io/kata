package vector

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const reconcilerLeasePollInterval = 100 * time.Millisecond

// AcquireReconcilerLease elects one semantic-search reconciler for a
// PostgreSQL database/schema pair. SQLite indexes are process-local and need
// no lease. A PostgreSQL lease is held on a dedicated connection until the
// returned release function is called.
func (ix *Index) AcquireReconcilerLease(ctx context.Context) (func() error, error) {
	if ix.pg == nil {
		return func() error { return nil }, nil
	}
	conn, err := ix.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("vector: reserve reconciler lease connection: %w", err)
	}
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, reconcilerLockSQL).Scan(&acquired); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("vector: acquire postgres reconciler lease: %w", err)
		}
		if acquired {
			break
		}
		timer := time.NewTimer(reconcilerLeasePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = conn.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := ix.pg.adoptLease(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() error {
		ix.pg.releaseLease(conn)
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := conn.ExecContext(releaseCtx, reconcilerUnlockSQL)
		return errors.Join(unlockErr, conn.Close())
	}, nil
}

// ValidateReconcilerLease checks that the exact PostgreSQL session used for
// derived-state mutations still owns the schema's reconciler lease. It asks
// reconcilerExecutor for that session precisely so it can never validate a
// different connection than the one a mutation would run on. SQLite
// reconciliation has no cross-process lease and always succeeds.
func (ix *Index) ValidateReconcilerLease(ctx context.Context) error {
	if ix.pg == nil {
		return nil
	}
	executor, err := ix.pg.reconcilerExecutor()
	if err != nil {
		return err
	}
	var held bool
	if err := executor.QueryRowContext(ctx, reconcilerLeaseHeldSQL).Scan(&held); err != nil {
		return fmt.Errorf("vector: validate postgres reconciler lease: %w", err)
	}
	if !held {
		return errors.New("vector: postgres reconciler lease was lost")
	}
	return nil
}

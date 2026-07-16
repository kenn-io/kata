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
	const lockQuery = `
		SELECT pg_try_advisory_lock(
			hashtext(current_database()),
			hashtext('kata:vector:reconciler:' || current_schema())
		)`
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, lockQuery).Scan(&acquired); err != nil {
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
	ix.pg.leaseMu.Lock()
	if ix.pg.leaseConn != nil {
		ix.pg.leaseMu.Unlock()
		_ = conn.Close()
		return nil, errors.New("vector: postgres reconciler lease already held by this index")
	}
	ix.pg.leaseConn = conn
	ix.pg.leaseMu.Unlock()
	return func() error {
		ix.pg.leaseMu.Lock()
		if ix.pg.leaseConn == conn {
			ix.pg.leaseConn = nil
		}
		ix.pg.leaseMu.Unlock()
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, unlockErr := conn.ExecContext(releaseCtx, `
			SELECT pg_advisory_unlock(
				hashtext(current_database()),
				hashtext('kata:vector:reconciler:' || current_schema())
			)`)
		return errors.Join(unlockErr, conn.Close())
	}, nil
}

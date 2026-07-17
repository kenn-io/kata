package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// AcquireFederationRunnerLease elects one federation runner for this
// PostgreSQL database/schema pair. The advisory lock is session-scoped and is
// held across each runner's complete poll/apply/cursor cycle.
func (s *Store) AcquireFederationRunnerLease(ctx context.Context) (func() error, error) {
	conn, err := s.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve postgres federation runner lease connection: %w", err)
	}
	lockName := "kata:federation:runner:" + s.schema
	if _, err := conn.ExecContext(ctx, `
		SELECT pg_catalog.pg_advisory_lock(
			pg_catalog.hashtext(pg_catalog.current_database()),
			pg_catalog.hashtext($1)
		)`, lockName); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("acquire postgres federation runner lease: %w", err)
	}

	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, unlockErr := conn.ExecContext(releaseCtx, `
				SELECT pg_catalog.pg_advisory_unlock(
					pg_catalog.hashtext(pg_catalog.current_database()),
					pg_catalog.hashtext($1)
				)`, lockName)
			releaseErr = errors.Join(unlockErr, conn.Close())
		})
		return releaseErr
	}, nil
}

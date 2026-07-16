package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const idempotencyLockPollInterval = 25 * time.Millisecond

// AcquireIdempotencyLock holds a schema/project/key-scoped advisory lock on a
// dedicated PostgreSQL session. Unsuccessful polls return their connection to
// the pool so a burst of identical retries cannot exhaust unrelated traffic.
func (s *Store) AcquireIdempotencyLock(
	ctx context.Context,
	projectID int64,
	key string,
) (func() error, error) {
	if key == "" {
		return func() error { return nil }, nil
	}
	if s.idempotencyDB == nil {
		return nil, errors.New("postgres idempotency coordinator is unavailable")
	}
	lockIdentity := fmt.Sprintf("kata:pgstore:idempotency:%s:%d:%s", s.schema, projectID, key)
	for {
		conn, err := s.idempotencyDB.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("reserve postgres idempotency lock connection: %w", err)
		}
		var acquired bool
		err = conn.QueryRowContext(ctx,
			`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, lockIdentity,
		).Scan(&acquired)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("acquire postgres idempotency lock: %w", mapSQLError(err, nil))
		}
		if acquired {
			return func() error {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, unlockErr := conn.ExecContext(releaseCtx,
					`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockIdentity)
				return errors.Join(mapSQLError(unlockErr, nil), conn.Close())
			}, nil
		}
		_ = conn.Close()
		timer := time.NewTimer(idempotencyLockPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

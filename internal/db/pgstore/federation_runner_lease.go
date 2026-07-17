package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

const federationRunnerLeasePollInterval = 100 * time.Millisecond

type federationRunnerLeaseState struct {
	mu   sync.RWMutex
	conn leaseQueryer
}

type leaseQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	Close() error
}

// AcquireFederationRunnerLease elects one federation runner for this
// PostgreSQL database/schema pair. The advisory lock is session-scoped and is
// held across each runner's complete poll/apply/cursor cycle.
func (s *Store) AcquireFederationRunnerLease(ctx context.Context) (func() error, error) {
	lockName := "kata:federation:runner:" + s.schema
	for {
		conn, err := s.Conn(ctx)
		if err != nil {
			if waitErr := waitForFederationRunnerLeaseRetry(ctx); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		var acquired bool
		err = conn.QueryRowContext(ctx, `
		SELECT pg_catalog.pg_try_advisory_lock(
			pg_catalog.hashtext(pg_catalog.current_database()),
			pg_catalog.hashtext($1)
		)`, lockName).Scan(&acquired)
		if err != nil || !acquired {
			_ = conn.Close()
			if waitErr := waitForFederationRunnerLeaseRetry(ctx); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		s.federationLease.mu.Lock()
		if s.federationLease.conn != nil {
			s.federationLease.mu.Unlock()
			_ = conn.Close()
			return nil, errors.New("postgres federation runner lease already held by this store")
		}
		s.federationLease.conn = conn
		s.federationLease.mu.Unlock()

		var once sync.Once
		var releaseErr error
		return func() error {
			once.Do(func() {
				s.federationLease.mu.Lock()
				if s.federationLease.conn == conn {
					s.federationLease.conn = nil
				}
				s.federationLease.mu.Unlock()
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
}

// ValidateFederationRunnerLease verifies the exact session used to acquire
// leadership is still alive and owns the schema's advisory lock.
func (s *Store) ValidateFederationRunnerLease(ctx context.Context) error {
	s.federationLease.mu.RLock()
	conn := s.federationLease.conn
	s.federationLease.mu.RUnlock()
	if conn == nil {
		return errors.New("postgres federation runner lease is not held")
	}
	lockName := "kata:federation:runner:" + s.schema
	var held bool
	err := conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM pg_catalog.pg_locks
			 WHERE pid = pg_catalog.pg_backend_pid()
			   AND locktype = 'advisory'
			   AND granted
			   AND classid = (pg_catalog.hashtext(pg_catalog.current_database())::bigint & 4294967295)
			   AND objid = (pg_catalog.hashtext($1)::bigint & 4294967295)
		)`, lockName).Scan(&held)
	if err != nil {
		return fmt.Errorf("validate postgres federation runner lease: %w", err)
	}
	if !held {
		return errors.New("postgres federation runner lease was lost")
	}
	return nil
}

func waitForFederationRunnerLeaseRetry(ctx context.Context) error {
	timer := time.NewTimer(federationRunnerLeasePollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

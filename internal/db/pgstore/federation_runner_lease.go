package pgstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const federationRunnerLeasePollInterval = 100 * time.Millisecond

type federationRunnerLeaseState struct {
	// A lease connection is a single PostgreSQL session. Serialize every query
	// and release operation so the runner and its monitor never drive pgx on
	// that session concurrently.
	mu   sync.Mutex
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
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !isFederationRunnerLeaseRetryable(err) {
				return nil, fmt.Errorf("reserve postgres federation runner lease connection: %w", mapSQLError(err, nil))
			}
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
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !isFederationRunnerLeaseRetryable(err) {
				return nil, fmt.Errorf("try postgres federation runner lease: %w", mapSQLError(err, nil))
			}
			if waitErr := waitForFederationRunnerLeaseRetry(ctx); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if !acquired {
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
				defer s.federationLease.mu.Unlock()
				if s.federationLease.conn == conn {
					s.federationLease.conn = nil
				}
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

func isFederationRunnerLeaseRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || pgconn.SafeToRetry(err) || IsTransient(err) {
		return true
	}
	var state interface{ SQLState() string }
	if !errors.As(err, &state) {
		return false
	}
	code := state.SQLState()
	return strings.HasPrefix(code, "08") || code == "57P01" || code == "57P02" || code == "57P03"
}

// ValidateFederationRunnerLease verifies the exact session used to acquire
// leadership is still alive and owns the schema's advisory lock.
func (s *Store) ValidateFederationRunnerLease(ctx context.Context) error {
	s.federationLease.mu.Lock()
	defer s.federationLease.mu.Unlock()
	conn := s.federationLease.conn
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

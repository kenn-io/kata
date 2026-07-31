package pgstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// AcquireFederationProjectSharedLock holds a schema/project-scoped advisory
// lock for one transport operation. Rebind takes the exclusive form, so the
// lock drains work across every daemon process sharing this PostgreSQL schema.
func (s *Store) AcquireFederationProjectSharedLock(
	ctx context.Context,
	projectID int64,
) (func(), error) {
	return s.acquireFederationProjectLock(ctx, projectID, true)
}

// AcquireFederationProjectExclusiveLock drains shared transport operations
// across every daemon process sharing this PostgreSQL schema.
func (s *Store) AcquireFederationProjectExclusiveLock(
	ctx context.Context,
	projectID int64,
) (func(), error) {
	return s.acquireFederationProjectLock(ctx, projectID, false)
}

func (s *Store) acquireFederationProjectLock(
	ctx context.Context,
	projectID int64,
	shared bool,
) (func(), error) {
	if s.idempotencyDB == nil {
		return nil, errors.New("postgres federation project coordinator is unavailable")
	}
	tx, err := s.idempotencyDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin postgres federation project lock: %w", err)
	}
	lockQuery := `
		SELECT pg_catalog.pg_advisory_xact_lock(
			pg_catalog.hashtextextended(pg_catalog.current_database() || ':' || $1, 0)
		)`
	if shared {
		lockQuery = `
			SELECT pg_catalog.pg_advisory_xact_lock_shared(
				pg_catalog.hashtextextended(pg_catalog.current_database() || ':' || $1, 0)
			)`
	}
	lockIdentity := fmt.Sprintf("kata:federation:project:%s:%d", s.schema, projectID)
	if _, err := tx.ExecContext(ctx, lockQuery, lockIdentity); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("acquire postgres federation project lock: %w", mapSQLError(err, nil))
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = tx.Rollback()
		})
	}, nil
}

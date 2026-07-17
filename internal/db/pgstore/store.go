// Package pgstore is the Postgres-backed implementation of db.Storage.
// Open opens via pgx's database/sql wrapper, applies sensible per-connection
// runtime params, bootstraps the canonical schema if the DB is fresh, and
// returns a ready-to-use *Store.
//
// Every db.Storage method has behavioral parity coverage shared with SQLite.
// stubs_gen.go is regenerated from internal/db/storage.go by ./stubgen and
// retains the compile-time interface assertion plus an inventory guard for
// future interface growth.
package pgstore

//go:generate go run ./stubgen

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib" // register pgx as the "pgx" sql driver

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// Store wraps a pgx-backed *sql.DB. Use Open to construct one with pool +
// runtime-param defaults applied. The dsn field carries the connection string
// for identity/diagnostics only — Path() returns the credential-free identity
// derived from it.
type Store struct {
	*sql.DB
	dsn                  string
	schema               string
	schemaOwner          string
	instanceUID          string
	readOnly             bool
	installedFreshSchema bool
	servingConn          *sql.Conn
	federationLease      *federationRunnerLeaseState
	idempotencyDB        *sql.DB
	exportQ              exportQueryer
}

// Close releases a process-lifetime serving lease before closing the pool.
func (s *Store) Close() error {
	var leaseErr error
	if s.servingConn != nil {
		leaseErr = s.servingConn.Close()
		s.servingConn = nil
	}
	var idempotencyErr error
	if s.idempotencyDB != nil {
		idempotencyErr = s.idempotencyDB.Close()
		s.idempotencyDB = nil
	}
	return errors.Join(leaseErr, idempotencyErr, s.DB.Close())
}

type exportQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// BeginExportSnapshot pins every Export* iterator to one read-only,
// repeatable-read transaction. The caller must invoke the returned release
// function before discarding the snapshot.
func (s *Store) BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error) {
	tx, err := s.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("begin export snapshot: %w", mapSQLError(err, nil))
	}
	snapshot := *s
	snapshot.exportQ = tx
	return &snapshot, tx.Rollback, nil
}

func (s *Store) exportQueryContext(
	ctx context.Context,
	query string,
	args ...any,
) (*sql.Rows, error) {
	if s.exportQ != nil {
		return s.exportQ.QueryContext(ctx, query, args...)
	}
	return s.QueryContext(ctx, query, args...)
}

// Path returns the credential-free DSN identity. Never returns the raw DSN —
// it could carry a password. Falls back to RedactDSN when CanonicalDSNIdentity
// errors so a logged Path() value never exposes a secret.
func (s *Store) Path() string {
	if id, err := config.CanonicalDSNIdentity(s.dsn); err == nil {
		return id
	}
	return config.RedactDSN(s.dsn)
}

// InstanceUID returns the cached meta.instance_uid value, populated on Open
// when the meta table exists (fresh DBs are bootstrapped and seeded in the
// same call). Empty only on a read-only handle that hasn't run cacheInstanceUID.
func (s *Store) InstanceUID() string { return s.instanceUID }

// InstalledFreshSchema reports whether this open atomically created the
// configured schema. Import cleanup uses this ownership proof so it never
// drops a schema that predated the importing process.
func (s *Store) InstalledFreshSchema() bool { return s.installedFreshSchema }

// RefreshInstanceUID re-reads meta.instance_uid into the cached field. Used
// after jsonl.Import overwrites the row.
func (s *Store) RefreshInstanceUID(ctx context.Context) error {
	var v string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v); err != nil {
		return fmt.Errorf("refresh instance_uid: %w", err)
	}
	s.instanceUID = v
	return nil
}

// SchemaVersion reads meta.schema_version. Errors when missing or unparseable.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// RetryTransient retries a complete operation for the bounded set of
// retry-safe Postgres SQLSTATEs. Callers must not wrap a single statement from
// an already-open transaction; use the transaction helpers instead.
func (s *Store) RetryTransient(ctx context.Context, op func() error) error {
	return db.RetryTransient(ctx, IsTransient, op)
}

// PeekSchemaVersion opens dsn read-only, reads meta.schema_version (or 0 if
// the meta table is absent), and closes the handle. Used by storeopen's
// pre-Open schema-shape check.
func PeekSchemaVersion(ctx context.Context, dsn string) (int, error) {
	return PeekSchemaVersionWithConfig(ctx, dsn, DefaultConfig())
}

// PeekSchemaVersionWithConfig reads the version from the selected schema
// without validating or mutating its lifecycle state.
func PeekSchemaVersionWithConfig(ctx context.Context, dsn string, pgConfig Config) (int, error) {
	pgConfig.SchemaMode = SchemaModeValidate
	if err := pgConfig.Validate(); err != nil {
		return 0, err
	}
	s, err := openInternal(ctx, dsn, pgConfig, true, false, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = s.Close() }()
	return s.currentVersion(ctx)
}

// currentVersion returns 0 when the meta table is absent or schema_version is
// missing, mirroring sqlitestore's connCurrentVersion contract.
func (s *Store) currentVersion(ctx context.Context) (int, error) {
	exists, err := s.tableExists(ctx, "meta")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v string
	err = s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// tableExists checks information_schema for a table in the current schema.
func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = $1)`, name).Scan(&exists)
	return exists, err
}

// cacheInstanceUIDIfPresent populates s.instanceUID when the meta table
// already holds an instance_uid row. Used by read-only Open on existing DBs.
func (s *Store) cacheInstanceUIDIfPresent(ctx context.Context) error {
	exists, err := s.tableExists(ctx, "meta")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var v string
	err = s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	s.instanceUID = v
	return nil
}

// Package storeopen routes a DSN to the right db.Storage backend. Bare paths
// and sqlite:// DSNs open the SQLite backend; postgres:// and postgresql://
// DSNs open the Postgres backend via internal/db/pgstore.
//
// Open is the single entry point that orchestrates migration: with
// db.ApplyMigrations(), it peeks the schema, runs JSONL cutover for SQLite
// DBs below db.BaselineSchemaVersion, opens the backend, and calls
// Storage.Migrate to apply pending steps. Without that option, a stale or
// missing DB surfaces db.ErrSchemaOutOfDate pointing the user at
// `kata migrate`.
package storeopen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// Open selects a storage backend from the DSN and returns a db.Storage along
// with the result of any migration that ran (zero-valued when nothing did).
// See the package doc for the full orchestration contract.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if cfg.ReadOnly {
		// A read-only handle cannot migrate; silently drop ApplyMigrations.
		cfg.ApplyMigrations = false
	}

	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		// Build the normalized option list from the post-ReadOnly cfg so
		// pgstore's branch picks up the same options the SQLite branch
		// does, instead of the raw input slice (which can still carry
		// ApplyMigrations alongside ReadOnly even after the silent-drop).
		return openPostgres(ctx, dsn, cfg)
	case hasScheme && scheme != "sqlite":
		return nil, db.MigrationResult{}, fmt.Errorf("unsupported dsn scheme %q", scheme)
	}

	path := dsn
	if hasScheme {
		path = strings.TrimPrefix(dsn, "sqlite://")
	}

	if cfg.ApplyMigrations {
		return openSQLiteWithMigrations(ctx, path, opts)
	}
	return openSQLiteRefusingStale(ctx, path, opts)
}

// OpenReadOnly opens the backend selected by dsn read-only. Any
// ApplyMigrations option is dropped because a mode=ro handle cannot mutate
// schema.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, db.MigrationResult, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

// openPostgres dispatches to pgstore with the normalized OpenConfig. The cfg
// argument is what Open already de-conflicted (ReadOnly clears
// ApplyMigrations); the helper re-derives the options list so pgstore.Open
// sees the same flags this orchestration layer enforced.
func openPostgres(ctx context.Context, dsn string, cfg db.OpenConfig) (db.Storage, db.MigrationResult, error) {
	pgOpts := normalizedOptions(cfg)
	if cfg.ApplyMigrations {
		return openPostgresWithMigrations(ctx, dsn, pgOpts)
	}
	return openPostgresRefusingStale(ctx, dsn, pgOpts)
}

func openPostgresWithMigrations(ctx context.Context, dsn string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	store, err := pgstore.Open(ctx, dsn, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	result, err := store.Migrate(ctx)
	if err != nil {
		_ = store.Close()
		return nil, db.MigrationResult{}, err
	}
	return store, result, nil
}

func openPostgresRefusingStale(ctx context.Context, dsn string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := pgstore.PeekSchemaVersion(ctx, dsn)
	switch {
	case peekErr != nil:
		// Postgres reachability / auth issues surface here. No "missing
		// file" sentinel maps to PG; propagate the error verbatim
		// (pgstore.Open already redacts the DSN out of its error chain
		// because pgx parses it before connecting).
		return nil, db.MigrationResult{}, peekErr
	case ver == 0:
		// PG with no meta table — no JSONL cutover path for PG. Refuse
		// and point the operator at `kata migrate` (same shape as the
		// SQLite missing-DB sentinel).
		return nil, db.MigrationResult{}, fmt.Errorf("%w: postgres database has no kata schema; run kata migrate", db.ErrSchemaOutOfDate)
	case ver > db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d; use a newer kata binary", ver, db.CurrentSchemaVersion())
	case ver != db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("%w: schema_version %d, current %d; run kata migrate", db.ErrSchemaOutOfDate, ver, db.CurrentSchemaVersion())
	}
	store, err := pgstore.Open(ctx, dsn, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	return store, db.MigrationResult{}, nil
}

// normalizedOptions rebuilds the OpenOption slice from a post-Open OpenConfig.
// Used to project the orchestration layer's view of the options back into a
// shape the backend Open functions accept.
func normalizedOptions(cfg db.OpenConfig) []db.OpenOption {
	var out []db.OpenOption
	if cfg.ReadOnly {
		out = append(out, db.ReadOnly())
	}
	if cfg.ApplyMigrations {
		out = append(out, db.ApplyMigrations())
	}
	return out
}

func openSQLiteWithMigrations(ctx context.Context, path string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr == nil && ver > db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, path, db.CurrentSchemaVersion())
	case peekErr == nil && ver > 0 && ver < db.BaselineSchemaVersion:
		if err := jsonl.AutoCutover(ctx, path); err != nil {
			return nil, db.MigrationResult{}, err
		}
		// Re-peek to confirm cutover advanced the version above the
		// cutover floor; surface the error from peek but discard the
		// value since the next Open re-reads it anyway.
		if _, err := sqlitestore.PeekSchemaVersion(ctx, path); err != nil {
			return nil, db.MigrationResult{}, fmt.Errorf("peek after cutover: %w", err)
		}
	case peekErr != nil && !isFileNotExist(peekErr):
		return nil, db.MigrationResult{}, peekErr
	}
	store, err := sqlitestore.Open(ctx, path, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	result, err := store.Migrate(ctx)
	if err != nil {
		_ = store.Close()
		return nil, db.MigrationResult{}, err
	}
	return store, result, nil
}

func openSQLiteRefusingStale(ctx context.Context, path string, opts []db.OpenOption) (db.Storage, db.MigrationResult, error) {
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr != nil && isFileNotExist(peekErr):
		return nil, db.MigrationResult{}, fmt.Errorf("%w: database does not exist at %s; run kata migrate", db.ErrSchemaOutOfDate, path)
	case peekErr != nil:
		return nil, db.MigrationResult{}, peekErr
	case ver > db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary", ver, path, db.CurrentSchemaVersion())
	case ver != db.CurrentSchemaVersion():
		return nil, db.MigrationResult{}, fmt.Errorf("%w: schema_version %d, current %d; run kata migrate", db.ErrSchemaOutOfDate, ver, db.CurrentSchemaVersion())
	}
	store, err := sqlitestore.Open(ctx, path, opts...)
	if err != nil {
		return nil, db.MigrationResult{}, err
	}
	return store, db.MigrationResult{}, nil
}

func isFileNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	// modernc.org/sqlite reports SQLITE_CANTOPEN as a string-suffix on the
	// wrapped error chain for missing files; the most portable check is
	// substring against the canonical message.
	return strings.Contains(err.Error(), "unable to open database file")
}

func splitScheme(dsn string) (scheme, rest string, hasScheme bool) {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return "", dsn, false
	}
	return dsn[:i], dsn[i+len("://"):], true
}

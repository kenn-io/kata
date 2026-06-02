// Package storeopen routes a DSN to the right db.Storage backend. Bare paths
// and sqlite:// DSNs open the SQLite backend. postgres:// and postgresql://
// DSNs are recognized but rejected until the Postgres backend implements the
// core db.Storage domain methods.
//
// Open peeks the on-disk schema, runs JSONL cutover for SQLite DBs whose
// schema_version predates db.CurrentSchemaVersion(), and hands the path to
// the backend's Open, which bootstraps a fresh DB from its canonical schema.sql
// inside a transaction. Every Open returns a ready-to-use Storage or a
// concrete error explaining why it couldn't.
package storeopen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// ErrPostgresNotSelectable reports that production callers cannot select
// pgstore through storeopen yet. The pgstore package remains available for
// schema/bootstrap tests, but its domain methods are still stubs.
var ErrPostgresNotSelectable = errors.New("postgres backend is not selectable until db.Storage methods are implemented")

// Open selects a storage backend from the DSN and returns a ready-to-use
// db.Storage. SQLite DSNs at a pre-current schema_version are upgraded through
// internal/jsonl.AutoCutover before the backend handle is opened.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	cfg := db.ApplyOpenOptions(opts...)
	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		return nil, ErrPostgresNotSelectable
	case hasScheme && scheme != "sqlite":
		return nil, fmt.Errorf("unsupported dsn scheme %q", scheme)
	}
	path := dsn
	if hasScheme {
		path = strings.TrimPrefix(dsn, "sqlite://")
	}
	return openSQLite(ctx, path, cfg, opts)
}

// OpenReadOnly opens the backend selected by dsn read-only. The cutover
// path is skipped because a read-only handle is the inspection path used
// during cutover itself.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

func openSQLite(ctx context.Context, path string, cfg db.OpenConfig, opts []db.OpenOption) (db.Storage, error) {
	if cfg.ReadOnly {
		return sqlitestore.Open(ctx, path, opts...)
	}
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr == nil && ver > db.CurrentSchemaVersion():
		return nil, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary",
			ver, path, db.CurrentSchemaVersion())
	case peekErr == nil && ver < db.CurrentSchemaVersion():
		if ver == 0 {
			hasTables, err := sqlitestore.HasUserTables(ctx, path)
			if err != nil {
				return nil, err
			}
			if !hasTables {
				break
			}
		}
		// Pre-current SQLite gets upgraded through JSONL cutover, which
		// exports the legacy shape and re-imports it into a fresh
		// baseline-shaped DB. Re-peek afterwards to confirm the cutover
		// landed; the error from peek surfaces if the on-disk state is
		// somehow worse than the source was.
		if err := jsonl.AutoCutover(ctx, path); err != nil {
			return nil, err
		}
		if _, err := sqlitestore.PeekSchemaVersion(ctx, path); err != nil {
			return nil, fmt.Errorf("peek after cutover: %w", err)
		}
	case peekErr != nil && !isFileNotExist(peekErr):
		return nil, peekErr
	}
	return sqlitestore.Open(ctx, path, opts...)
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

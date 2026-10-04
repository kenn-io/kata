// Package storeopen routes a DSN to the right db.Storage backend. Bare paths
// and sqlite:// DSNs open SQLite; postgres:// and postgresql:// DSNs open
// Postgres.
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

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitelock"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

// Backend identifies the storage implementation selected by a DSN.
type Backend string

const (
	// BackendSQLite selects the local SQLite implementation.
	BackendSQLite Backend = "sqlite"
	// BackendPostgres selects the Postgres implementation.
	BackendPostgres Backend = "postgres"
)

// Config carries backend-specific startup policy. Zero-value backend fields
// are replaced with that backend's standalone defaults.
type Config struct {
	Postgres pgstore.Config
	// RequireMigrationConsent refuses upgrades of existing SQLite databases.
	// The CLI sets this for development builds without explicit consent.
	// Library callers retain automatic upgrades with the zero value.
	RequireMigrationConsent bool
}

// ErrMigrationConsentRequired means startup policy refused an existing SQLite
// schema upgrade. The caller supplies instructions for granting consent.
var ErrMigrationConsentRequired = errors.New("SQLite migration requires consent")

// InstalledFreshPostgresSchema reports whether store's open created its
// configured Postgres schema. False is the safe answer for other backends and
// for Postgres schemas that already existed.
func InstalledFreshPostgresSchema(store db.Storage) bool {
	reporter, ok := store.(interface{ InstalledFreshSchema() bool })
	return ok && reporter.InstalledFreshSchema()
}

// DefaultConfig returns the standalone storage startup policy.
func DefaultConfig() Config {
	return Config{Postgres: pgstore.DefaultConfig()}
}

// BackendForDSN resolves a DSN without opening or mutating its target.
func BackendForDSN(dsn string) (Backend, error) {
	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case !hasScheme || scheme == "sqlite":
		return BackendSQLite, nil
	case scheme == "postgres" || scheme == "postgresql":
		return BackendPostgres, nil
	default:
		return "", fmt.Errorf("unsupported dsn scheme %q", scheme)
	}
}

// Validate reports whether dsn selects a storage backend this binary can
// start without opening or mutating the database.
func Validate(dsn string) error {
	_, err := BackendForDSN(dsn)
	return err
}

// Open selects a storage backend from the DSN and returns a ready-to-use
// db.Storage. SQLite DSNs at a pre-current schema_version are upgraded through
// internal/jsonl.AutoCutover before the backend handle is opened.
func Open(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	openConfig := DefaultConfig()
	backend, err := BackendForDSN(dsn)
	if err != nil {
		return nil, err
	}
	if backend == BackendPostgres {
		storageConfig, err := config.KataPostgresStorageConfig(ctx)
		if err != nil {
			return nil, err
		}
		openConfig.Postgres = pgstore.ConfigFromValues(
			storageConfig.Schema, storageConfig.Mode, storageConfig.SchemaOwner,
			storageConfig.AllowInsecure,
		)
	}
	return OpenWithConfig(ctx, dsn, openConfig, opts...)
}

// OpenWithConfig selects a backend while preserving backend-specific startup
// policy. SQLite ignores the Postgres field; Postgres validates it before
// connecting or attempting schema work.
func OpenWithConfig(ctx context.Context, dsn string, openConfig Config, opts ...db.OpenOption) (db.Storage, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if err := Validate(dsn); err != nil {
		return nil, err
	}
	backend, err := BackendForDSN(dsn)
	if err != nil {
		return nil, err
	}
	if backend == BackendPostgres {
		pgConfig := openConfig.Postgres
		if pgConfig == (pgstore.Config{}) {
			pgConfig = pgstore.DefaultConfig()
		}
		return pgstore.OpenWithConfig(ctx, dsn, pgConfig, opts...)
	}
	_, _, hasScheme := splitScheme(dsn)
	path := dsn
	if hasScheme {
		path = strings.TrimPrefix(dsn, "sqlite://")
	}
	return openSQLite(ctx, path, cfg, opts, openConfig.RequireMigrationConsent)
}

// PeekSchemaVersion reads a target's schema version without applying
// migrations. A missing Postgres schema reports version zero; SQLite keeps its
// existing missing-file error so callers can distinguish it from a v0 file.
func PeekSchemaVersion(ctx context.Context, dsn string) (int, error) {
	backend, err := BackendForDSN(dsn)
	if err != nil {
		return 0, err
	}
	if backend == BackendPostgres {
		storageConfig, err := config.KataPostgresStorageConfig(ctx)
		if err != nil {
			return 0, err
		}
		pgConfig := pgstore.ConfigFromValues(
			storageConfig.Schema, storageConfig.Mode, storageConfig.SchemaOwner,
			storageConfig.AllowInsecure,
		)
		return pgstore.PeekSchemaVersionWithConfig(ctx, dsn, pgConfig)
	}
	_, _, hasScheme := splitScheme(dsn)
	path := dsn
	if hasScheme {
		path = strings.TrimPrefix(dsn, "sqlite://")
	}
	return sqlitestore.PeekSchemaVersion(ctx, path)
}

// CheckMigrationConsent checks SQLite upgrade eligibility without creating or
// changing the database. Callers use it before stopping a running daemon; Open
// checks the same policy again while holding the database lock.
func CheckMigrationConsent(ctx context.Context, dsn string) error {
	backend, err := BackendForDSN(dsn)
	if err != nil || backend != BackendSQLite {
		return err
	}
	path := strings.TrimPrefix(dsn, "sqlite://")
	ver, needed, err := sqliteMigrationNeeded(ctx, path)
	if err != nil || !needed {
		return err
	}
	return sqliteMigrationConsentError(path, ver)
}

// RemoveFreshPostgresTarget rolls back a schema installed for a failed first
// import. pgstore verifies the exact instance identity and absence of domain
// state under exclusive table locks before dropping anything.
func RemoveFreshPostgresTarget(ctx context.Context, dsn, instanceUID string) error {
	backend, err := BackendForDSN(dsn)
	if err != nil {
		return err
	}
	if backend != BackendPostgres {
		return fmt.Errorf("fresh target cleanup requires a postgres DSN")
	}
	storageConfig, err := config.KataPostgresStorageConfig(ctx)
	if err != nil {
		return err
	}
	pgConfig := pgstore.ConfigFromValues(
		storageConfig.Schema, storageConfig.Mode, storageConfig.SchemaOwner,
		storageConfig.AllowInsecure,
	)
	return pgstore.RemoveFreshSchemaWithConfig(ctx, dsn, instanceUID, pgConfig)
}

// OpenReadOnly opens the backend selected by dsn read-only. The cutover
// path is skipped because a read-only handle is the inspection path used
// during cutover itself.
func OpenReadOnly(ctx context.Context, dsn string, opts ...db.OpenOption) (db.Storage, error) {
	return Open(ctx, dsn, append(opts, db.ReadOnly())...)
}

func openSQLite(ctx context.Context, path string, cfg db.OpenConfig, opts []db.OpenOption, requireConsent bool) (db.Storage, error) {
	if cfg.ReadOnly {
		return sqlitestore.Open(ctx, path, opts...)
	}
	lock, err := sqlitelock.Acquire(path)
	if err != nil {
		return nil, err
	}
	storage, err := openSQLiteDatabase(ctx, path, opts, requireConsent)
	if err != nil {
		lock.Release()
		return nil, err
	}
	return &lockedStorage{Store: storage, lock: lock}, nil
}

func openSQLiteDatabase(ctx context.Context, path string, opts []db.OpenOption, requireConsent bool) (*sqlitestore.Store, error) {
	ver, needed, err := sqliteMigrationNeeded(ctx, path)
	if err != nil {
		return nil, err
	}
	if needed {
		if requireConsent {
			return nil, sqliteMigrationConsentError(path, ver)
		}
		// Recheck after cutover before handing the replacement to the caller.
		if err := jsonl.AutoCutover(ctx, path); err != nil {
			return nil, err
		}
		if _, err := sqlitestore.PeekSchemaVersion(ctx, path); err != nil {
			return nil, fmt.Errorf("peek after cutover: %w", err)
		}
	}
	return sqlitestore.Open(ctx, path, opts...)
}

func sqliteMigrationConsentError(path string, ver int) error {
	return fmt.Errorf("%w: database %s from schema %d to %d", ErrMigrationConsentRequired, path, ver, db.CurrentSchemaVersion())
}

func sqliteMigrationNeeded(ctx context.Context, path string) (int, bool, error) {
	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	switch {
	case peekErr == nil && ver > db.CurrentSchemaVersion():
		return ver, false, fmt.Errorf("schema_version %d at %s is newer than binary schema %d; use a newer kata binary",
			ver, path, db.CurrentSchemaVersion())
	case peekErr == nil && ver < db.CurrentSchemaVersion():
		if ver == 0 {
			hasTables, err := sqlitestore.HasUserTables(ctx, path)
			if err != nil {
				return ver, false, err
			}
			if !hasTables {
				break
			}
		}
		return ver, true, nil
	case peekErr != nil && !isFileNotExist(peekErr):
		return ver, false, peekErr
	}
	return ver, false, nil
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
	before, after, ok := strings.Cut(dsn, "://")
	if !ok {
		return "", dsn, false
	}
	return before, after, true
}

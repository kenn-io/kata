// Package db opens the kata SQLite database and bootstraps it from the
// canonical schema.sql. Older databases reach the current shape via JSONL
// cutover (internal/jsonl/cutover.go) — there is no incremental migration
// ladder in this package.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"

	katauid "go.kenn.io/kata/internal/uid"
)

//go:embed schema.sql
var schemaSQL string

const currentSchemaVersion = 12

// CurrentSchemaVersion returns the schema version expected by this binary.
func CurrentSchemaVersion() int { return currentSchemaVersion }

// ErrSchemaCutoverRequired is returned by Open when an existing database is
// older than the binary's schema and must be upgraded through JSONL cutover.
var ErrSchemaCutoverRequired = errors.New("schema cutover required")

// DB wraps *sql.DB. Use Open to construct one with PRAGMAs applied.
type DB struct {
	*sql.DB
	path        string
	instanceUID string
	readOnly    bool
}

// Open opens (and if needed initializes) the kata SQLite database at path.
// PRAGMAs are applied for every connection (via the connection string and
// post-open exec). Fresh databases are bootstrapped from schema.sql inside a
// transaction; older databases return ErrSchemaCutoverRequired so the daemon
// can run JSONL cutover before reopening. Open is the single authoritative
// writer of meta.instance_uid outside an import transaction: after bootstrap,
// if the row is absent it generates one via uid.New(). The cached value is
// exposed via InstanceUID for insert paths.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)",
		path,
	)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Single writer is fine for v1; SetMaxOpenConns left at default for reads.
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	d := &DB{DB: sdb, path: path}
	if err := d.bootstrap(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := d.ensureInstanceUID(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	if err := d.EnsureSystemProject(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return d, nil
}

// InstanceUID returns the local kata installation's stable identifier. The
// value is read once at Open and used to stamp origin_instance_uid on every
// event and purge_log row written by this daemon.
func (d *DB) InstanceUID() string { return d.instanceUID }

// RefreshInstanceUID re-reads meta.instance_uid into the cached field. Used by
// jsonl.Import after commit so that a default-mode v3 import — which
// overwrites meta.instance_uid with the source's value inside the import
// transaction — leaves the cached value in sync with the row. Without this,
// the handle would internally disagree (SQL says SOURCE_INSTANCE; cached says
// the pre-import LOCAL_FRESH) and any subsequent event/purge insert on the
// same handle would stamp the wrong origin_instance_uid.
func (d *DB) RefreshInstanceUID(ctx context.Context) error {
	var v string
	if err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v); err != nil {
		return fmt.Errorf("refresh instance_uid: %w", err)
	}
	d.instanceUID = v
	return nil
}

// ensureInstanceUID is the single ownership rule for meta.instance_uid: if the
// row is absent it is inserted with a fresh ULID; if present it is read into
// d.instanceUID. Idempotent across reboots and across every Open caller (tests,
// import target init, cutover temp DB).
//
// Existing DBs take the read-only fast path: a single SELECT, no write. Only
// when the row is absent (fresh DB) do we generate a UID and run INSERT ...
// ON CONFLICT DO NOTHING followed by a SELECT to recover whichever value won
// a concurrent first-open race — the losing writer's INSERT is a no-op.
func (d *DB) ensureInstanceUID(ctx context.Context) error {
	var existing string
	err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&existing)
	if err == nil {
		d.instanceUID = existing
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('instance_uid', ?)
		 ON CONFLICT(key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	d.instanceUID = stored
	return nil
}

// OpenReadOnly opens an existing kata database without bootstrapping.
// It is used by JSONL cutover so the old source DB can be exported without
// the normal Open path mutating meta.schema_version first.
func OpenReadOnly(ctx context.Context, path string) (*DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
		path,
	)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only %s: %w", path, err)
	}
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping read-only %s: %w", path, err)
	}
	return &DB{DB: sdb, path: path, readOnly: true}, nil
}

// Checkpoint runs a truncating WAL checkpoint for writable databases. It is
// used by graceful shutdown paths so the durable main database file is brought
// up to date before the process exits.
func (d *DB) Checkpoint(ctx context.Context) error {
	if d.readOnly {
		return nil
	}
	var busy, logFrames, checkpointedFrames int
	if err := d.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return fmt.Errorf("checkpoint WAL: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint WAL: busy=%d log=%d checkpointed=%d",
			busy, logFrames, checkpointedFrames)
	}
	return nil
}

// Close checkpoints writable WAL databases before closing the connection pool.
// Read-only handles skip the checkpoint because SQLite rejects WAL checkpoints
// on read-only connections.
func (d *DB) Close() error {
	if d == nil || d.DB == nil {
		return nil
	}
	var checkpointErr error
	if !d.readOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		checkpointErr = d.Checkpoint(ctx)
		cancel()
	}
	return errors.Join(checkpointErr, d.DB.Close())
}

// Path returns the resolved database path.
func (d *DB) Path() string { return d.path }

// PeekSchemaVersion reads meta.schema_version without bootstrapping the DB.
// It returns 0 when the database exists but has no meta table or schema_version
// row.
func PeekSchemaVersion(ctx context.Context, path string) (int, error) {
	d, err := OpenReadOnly(ctx, path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.Close() }()
	return d.currentVersion(ctx)
}

// bootstrap initializes a fresh database from schema.sql or refuses to open an
// older database. Existing databases at the current schema version are left
// untouched; older databases return ErrSchemaCutoverRequired so the daemon
// startup path can run JSONL cutover; newer databases are an unrecoverable
// state for this binary.
func (d *DB) bootstrap(ctx context.Context) error {
	current, err := d.currentVersion(ctx)
	if err != nil {
		return err
	}
	if current > currentSchemaVersion {
		return fmt.Errorf("database schema_version %d is newer than binary schema %d",
			current, currentSchemaVersion)
	}
	if current > 0 && current < currentSchemaVersion {
		return fmt.Errorf("%w: database schema_version %d is older than binary schema %d; run JSONL cutover before opening",
			ErrSchemaCutoverRequired, current, currentSchemaVersion)
	}
	if current == currentSchemaVersion {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin schema bootstrap: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("apply schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(currentSchemaVersion)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record schema version %d: %w", currentSchemaVersion, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return nil
}

// currentVersion returns 0 when the meta table doesn't exist yet (fresh DB).
func (d *DB) currentVersion(ctx context.Context) (int, error) {
	exists, err := d.tableExists(ctx, "meta")
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v string
	err = d.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
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

func (d *DB) tableExists(ctx context.Context, name string) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

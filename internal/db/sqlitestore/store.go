// Package sqlitestore opens the kata SQLite database and bootstraps it from
// the canonical schema.sql. A fresh DB lands at db.CurrentSchemaVersion() in
// one transaction; an existing DB at the current version opens unchanged; an
// older DB returns ErrSchemaCutoverRequired so storeopen can drive a JSONL
// cutover before reopening.
package sqlitestore

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver registered as "sqlite"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

//go:embed schema.sql
var schemaSQL string

// ErrSchemaCutoverRequired reports that an existing database is older than
// the binary's schema and must be upgraded through JSONL cutover before it
// can be opened. storeopen converts this into a cutover-then-reopen flow.
var ErrSchemaCutoverRequired = errors.New("schema cutover required")

// Store wraps *sql.DB. Use Open to construct one with PRAGMAs applied and the
// schema bootstrapped.
//
// readQ is the queryable used by streaming read iterators (Export*). It
// defaults to the embedded *sql.DB. BeginExportSnapshot returns a *Store
// copy with readQ swapped for a read-only *sql.Tx so an Export ranges a
// consistent snapshot even if other writers commit mid-export.
type Store struct {
	*sql.DB
	path        string
	instanceUID string
	readOnly    bool
	readQ       readQuerier
}

// readQuerier is the read-only query surface shared between *sql.DB and *sql.Tx.
// Streaming iterators use this so a snapshot-bound tx can be swapped in.
type readQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Compile-time check that *Store satisfies the neutral db.Storage contract.
var _ db.Storage = (*Store)(nil)

// Open opens (and if needed initializes) the kata SQLite database at path.
// PRAGMAs are applied for every connection via the connection string. Fresh
// databases are bootstrapped from schema.sql inside a transaction; older
// databases return ErrSchemaCutoverRequired so the storeopen path can run
// JSONL cutover before reopening; newer databases are an unrecoverable state
// for this binary. Open is the single authoritative writer of
// meta.instance_uid outside an import transaction: after bootstrap, if the
// row is absent it generates one via uid.New(). The cached value is exposed
// via InstanceUID for insert paths.
//
// Pass db.ReadOnly() to open an existing database without bootstrapping or
// PRAGMA writes. The cutover and preflight paths use this to inspect an old
// source DB before the destructive replace.
func Open(ctx context.Context, path string, opts ...db.OpenOption) (*Store, error) {
	cfg := db.ApplyOpenOptions(opts...)
	if cfg.ReadOnly {
		return openReadOnly(ctx, path)
	}
	synchronous := "NORMAL"
	pragmas := []string{
		"_pragma=foreign_keys(1)",
		"_pragma=journal_mode(WAL)",
	}
	if fastSQLiteForTestHarness() {
		synchronous = "OFF"
		pragmas = append(pragmas, "_pragma=temp_store(MEMORY)")
	}
	pragmas = append(pragmas,
		fmt.Sprintf("_pragma=synchronous(%s)", synchronous),
		"_pragma=busy_timeout(5000)",
	)
	dsn := fmt.Sprintf("file:%s?%s", path, strings.Join(pragmas, "&"))
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// Single writer is fine for v1; SetMaxOpenConns left at default for reads.
	if err := sdb.PingContext(ctx); err != nil {
		_ = sdb.Close()
		return nil, fmt.Errorf("ping %s: %w", path, err)
	}
	d := &Store{DB: sdb, path: path}
	d.readQ = sdb
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

// BeginExportSnapshot returns a snapshot Storage whose Export* iterators all
// read through a single read-only transaction. The returned release function
// rolls the snapshot back; callers must call it before discarding the
// snapshot. Other (non-Export) methods on the returned store continue to use
// the underlying *sql.DB pool, which is intentional — Export iterators are
// the only readers that benefit from snapshot isolation today.
//
// The return type is db.Storage so this method satisfies the snapshotter
// interface jsonl.Export type-asserts.
func (d *Store) BeginExportSnapshot(ctx context.Context) (db.Storage, func() error, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("begin export snapshot: %w", err)
	}
	snap := *d
	snap.readQ = tx
	return &snap, tx.Rollback, nil
}

// InstanceUID returns the local kata installation's stable identifier. The
// value is read once at Open and used to stamp origin_instance_uid on every
// event and purge_log row written by this daemon.
func (d *Store) InstanceUID() string { return d.instanceUID }

// RefreshInstanceUID re-reads meta.instance_uid into the cached field. Used by
// jsonl.Import after commit so that a default-mode v3 import — which
// overwrites meta.instance_uid with the source's value inside the import
// transaction — leaves the cached value in sync with the row. Without this,
// the handle would internally disagree (SQL says SOURCE_INSTANCE; cached says
// the pre-import LOCAL_FRESH) and any subsequent event/purge insert on the
// same handle would stamp the wrong origin_instance_uid.
func (d *Store) RefreshInstanceUID(ctx context.Context) error {
	var v string
	if err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v); err != nil {
		return fmt.Errorf("refresh instance_uid: %w", err)
	}
	d.instanceUID = v
	return nil
}

// bootstrap initializes a fresh database from schema.sql or refuses to open
// an older database. An existing database at the current schema version is
// left untouched; older databases return ErrSchemaCutoverRequired so the
// storeopen path can run JSONL cutover; newer databases are an unrecoverable
// state for this binary.
func (d *Store) bootstrap(ctx context.Context) error {
	current, err := d.currentVersion(ctx)
	if err != nil {
		return err
	}
	currentBinary := db.CurrentSchemaVersion()
	if current > currentBinary {
		return fmt.Errorf("database schema_version %d is newer than binary schema %d",
			current, currentBinary)
	}
	if current > 0 && current < currentBinary {
		return fmt.Errorf("%w: database schema_version %d is older than binary schema %d; run JSONL cutover before opening",
			ErrSchemaCutoverRequired, current, currentBinary)
	}
	if current == currentBinary {
		return nil
	}
	hasTables, err := d.hasUserTables(ctx)
	if err != nil {
		return err
	}
	if hasTables {
		return fmt.Errorf("%w: existing database has schema_version 0; run JSONL cutover before opening",
			ErrSchemaCutoverRequired)
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
		strconv.Itoa(currentBinary)); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record schema version %d: %w", currentBinary, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema bootstrap: %w", err)
	}
	return nil
}

// ensureInstanceUID is the single ownership rule for meta.instance_uid: if
// the row is absent it is inserted with a fresh ULID; if present it is read
// into d.instanceUID. Idempotent across reboots and every Open caller (tests,
// import target init, cutover temp DB). Existing DBs take the read-only
// fast path: a single SELECT, no write.
func (d *Store) ensureInstanceUID(ctx context.Context) error {
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

// openReadOnly opens an existing kata database without bootstrapping. It is the
// implementation behind Open(..., db.ReadOnly()).
func openReadOnly(ctx context.Context, path string) (*Store, error) {
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
	s := &Store{DB: sdb, path: path, readOnly: true}
	s.readQ = sdb
	return s, nil
}

// Path returns the resolved database path.
func (d *Store) Path() string { return d.path }

// Checkpoint runs a truncating WAL checkpoint for writable databases. It is
// used by graceful shutdown paths so the durable main database file is brought
// up to date before the process exits. Read-only handles short-circuit: SQLite
// rejects WAL checkpoints on read-only connections.
func (d *Store) Checkpoint(ctx context.Context) error {
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
func (d *Store) Close() error {
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

// RetryTransient retries op while it returns a SQLite lock-contention error.
func (d *Store) RetryTransient(ctx context.Context, op func() error) error {
	return db.RetryTransient(ctx, IsTransient, op)
}

// testFastSQLiteEnv opts test binaries into reduced-durability PRAGMAs
// (synchronous=OFF, temp_store=MEMORY). Production binaries ignore it; the
// guard requires the env *and* an os.Args[0] basename ending in .test or
// .test.exe so a flag accidentally exported into a production shell can't
// degrade real-database durability.
const testFastSQLiteEnv = "KATA_TEST_FAST_SQLITE"

func fastSQLiteForTestHarness() bool {
	if os.Getenv(testFastSQLiteEnv) != "1" {
		return false
	}
	bin := strings.ToLower(filepath.Base(os.Args[0]))
	return strings.HasSuffix(bin, ".test") || strings.HasSuffix(bin, ".test.exe")
}

// PeekSchemaVersion reads meta.schema_version without bootstrapping the DB.
// It returns 0 when the database exists but has no meta table or
// schema_version row.
func PeekSchemaVersion(ctx context.Context, path string) (int, error) {
	d, err := Open(ctx, path, db.ReadOnly())
	if err != nil {
		return 0, err
	}
	defer func() { _ = d.Close() }()
	return d.currentVersion(ctx)
}

// HasUserTables reports whether an existing SQLite database contains any
// application/user tables. A zero schema_version plus user tables means
// "legacy or corrupt existing DB", not "fresh file"; storeopen uses this to
// avoid bootstrapping schema.sql over existing tables.
func HasUserTables(ctx context.Context, path string) (bool, error) {
	d, err := Open(ctx, path, db.ReadOnly())
	if err != nil {
		return false, err
	}
	defer func() { _ = d.Close() }()
	return d.hasUserTables(ctx)
}

// SchemaVersion returns the integer stored in meta.schema_version. It errors
// when the row is absent or unparseable (unlike currentVersion, which treats
// a missing meta table as version 0 for the bootstrap path). The read routes
// through readQ so jsonl.Export's snapshot-bound store sees this as the
// first read on its tx, pinning the snapshot here rather than at the
// iterators that follow.
func (d *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v string
	if err := d.readQ.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("parse schema_version %q: %w", v, err)
	}
	return n, nil
}

// currentVersion returns 0 when the meta table doesn't exist yet (fresh DB).
func (d *Store) currentVersion(ctx context.Context) (int, error) {
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

func (d *Store) tableExists(ctx context.Context, name string) (bool, error) {
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

func (d *Store) hasUserTables(ctx context.Context) (bool, error) {
	var n int
	err := d.QueryRowContext(ctx,
		`SELECT COUNT(*)
		   FROM sqlite_master
		  WHERE type='table'
		    AND name NOT LIKE 'sqlite_%'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// Migrate brings the SQLite database up to db.CurrentSchemaVersion() by
// applying every pending migration file from migrationsSource in version
// order. The recovery snapshot is taken before BEGIN IMMEDIATE because SQLite
// refuses VACUUM INTO inside a transaction; the lock then serializes concurrent
// migrators so only one applies pending steps. On any per-step error the
// transaction rolls back and the snapshot is retained — the error message
// names the snapshot path so an operator can restore.
func (d *Store) Migrate(ctx context.Context) (db.MigrationResult, error) {
	if d.readOnly {
		return db.MigrationResult{}, errors.New("migrate: backend is read-only")
	}

	current, err := d.currentVersion(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version: %w", err)
	}
	if current > 0 && current < db.BaselineSchemaVersion {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d); use storeopen.Open with db.ApplyMigrations() so JSONL cutover runs first", current, db.BaselineSchemaVersion)
	}
	if current > db.CurrentSchemaVersion() {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d; use a newer kata binary", current, db.CurrentSchemaVersion())
	}

	pending, err := listPendingMigrations(migrationsSource, current)
	if err != nil {
		return db.MigrationResult{}, err
	}
	if len(pending) == 0 {
		if err := d.ensureInstanceUIDFromMeta(ctx); err != nil {
			return db.MigrationResult{}, err
		}
		// EnsureSystemProject is idempotent. Migrate is the single
		// canonical place that ensures the system project exists; the
		// at-current no-op path runs it too so DBs that arrived at
		// current via other paths (e.g. cutover targets) still get the
		// seed.
		if err := d.EnsureSystemProject(ctx); err != nil {
			return db.MigrationResult{}, fmt.Errorf("ensure system project: %w", err)
		}
		return db.MigrationResult{From: current, To: current, Applied: nil}, nil
	}

	snapshotPath, err := d.takeRecoverySnapshot(ctx, current)
	if err != nil {
		return db.MigrationResult{}, err
	}

	// Pin a single connection so BEGIN IMMEDIATE/COMMIT live on the same
	// conn and we keep the rest of the Store's PRAGMAs untouched. Concurrent
	// migrators serialize on this BEGIN IMMEDIATE: the second caller blocks
	// (busy_timeout=5000ms via the connection-string PRAGMA) until the first
	// COMMITs, then re-reads the version inside the lock and finds the
	// ladder empty.
	conn, err := d.Conn(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("acquire migration conn: %w (snapshot retained at %s)", err, snapshotPath)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return db.MigrationResult{}, fmt.Errorf("begin immediate: %w (snapshot retained at %s)", err, snapshotPath)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	// Re-read schema_version inside the lock — another migrator may have
	// advanced it between the snapshot and the lock. Recompute pending so we
	// only apply what's still pending.
	currentInTx, err := connCurrentVersion(ctx, conn)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version in tx: %w (snapshot retained at %s)", err, snapshotPath)
	}
	if currentInTx > 0 && currentInTx < db.BaselineSchemaVersion {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d) under lock (snapshot retained at %s)", currentInTx, db.BaselineSchemaVersion, snapshotPath)
	}
	if currentInTx > db.CurrentSchemaVersion() {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d under lock (snapshot retained at %s)", currentInTx, db.CurrentSchemaVersion(), snapshotPath)
	}
	pendingInTx, err := listPendingMigrations(migrationsSource, currentInTx)
	if err != nil {
		return db.MigrationResult{}, err
	}

	applied := make([]int, 0, len(pendingInTx))
	for _, m := range pendingInTx {
		sqlBytes, err := fs.ReadFile(migrationsSource, m.fileName)
		if err != nil {
			return db.MigrationResult{}, fmt.Errorf("read migration %s: %w (snapshot retained at %s)", m.fileName, err, snapshotPath)
		}
		if _, err := conn.ExecContext(ctx, string(sqlBytes)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("apply migration %s: %w (snapshot retained at %s)", m.fileName, err, snapshotPath)
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO meta(key,value) VALUES('schema_version', ?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			strconv.Itoa(m.version)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("stamp schema_version %d: %w (snapshot retained at %s)", m.version, err, snapshotPath)
		}
		applied = append(applied, m.version)
	}

	if err := ensureInstanceUIDOnConn(ctx, conn, &d.instanceUID); err != nil {
		return db.MigrationResult{}, fmt.Errorf("ensure instance_uid: %w (snapshot retained at %s)", err, snapshotPath)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return db.MigrationResult{}, fmt.Errorf("commit migration: %w (snapshot retained at %s)", err, snapshotPath)
	}
	committed = true

	if rmErr := os.Remove(snapshotPath); rmErr != nil && !os.IsNotExist(rmErr) {
		// Best-effort: leaving the snapshot on disk wastes space but the
		// migration is still successful. Do not surface as an error.
		_ = rmErr
	}

	// EnsureSystemProject is idempotent and seeds the hidden project that
	// daemon-global token events bind to. With DDL ownership moved out of
	// Open, this is the canonical seeding point.
	if err := d.EnsureSystemProject(ctx); err != nil {
		return db.MigrationResult{}, fmt.Errorf("ensure system project: %w", err)
	}

	to := currentInTx
	if len(applied) > 0 {
		to = applied[len(applied)-1]
	}
	return db.MigrationResult{From: current, To: to, Applied: applied}, nil
}

type pendingMigration struct {
	version  int
	fileName string
}

// listPendingMigrations enumerates the migration ladder under "migrations/" in
// source, validates the ladder shape (no duplicate version prefixes, no gaps),
// and returns the subset whose version exceeds current. The validation runs
// before the gap check returns, so a corrupted embedded ladder is caught even
// when current is at the head.
func listPendingMigrations(source fs.FS, current int) ([]pendingMigration, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var all []pendingMigration
	seen := make(map[int]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		if len(name) < 4 {
			return nil, fmt.Errorf("migration %q: name too short to carry a version prefix", name)
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			return nil, fmt.Errorf("migration %q: parse version prefix: %w", name, err)
		}
		if existing, dup := seen[v]; dup {
			return nil, fmt.Errorf("migration version %d: duplicate files %q and %q", v, existing, name)
		}
		seen[v] = name
		// io/fs paths are always forward-slash; filepath.Join would emit
		// "migrations\name" on Windows and break fs.ReadFile.
		all = append(all, pendingMigration{version: v, fileName: path.Join("migrations", name)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for i := 1; i < len(all); i++ {
		if all[i].version != all[i-1].version+1 {
			return nil, fmt.Errorf("migration ladder: version %d missing between %d and %d", all[i-1].version+1, all[i-1].version, all[i].version)
		}
	}
	var pending []pendingMigration
	for _, m := range all {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

func (d *Store) takeRecoverySnapshot(ctx context.Context, fromVersion int) (string, error) {
	home, err := config.KataHome()
	if err != nil {
		return "", fmt.Errorf("resolve KATA_HOME: %w", err)
	}
	dir := filepath.Join(home, "runtime", config.DBHash(d.path))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}
	snapshot := filepath.Join(dir, fmt.Sprintf("premigrate-v%d.db", fromVersion))
	if err := os.Remove(snapshot); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("clear stale snapshot %s: %w", snapshot, err)
	}
	if _, err := d.ExecContext(ctx, fmt.Sprintf("VACUUM INTO %s", sqliteQuoteLiteral(snapshot))); err != nil {
		return "", fmt.Errorf("vacuum into %s: %w", snapshot, err)
	}
	return snapshot, nil
}

// sqliteQuoteLiteral wraps s in single quotes and doubles any single quotes
// inside it. Path-based; the migration code controls the input shape, so this
// guards against future surprises (e.g. KATA_HOME on a path with apostrophes).
func sqliteQuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func connCurrentVersion(ctx context.Context, conn *sql.Conn) (int, error) {
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v string
	err = conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// ensureInstanceUIDFromMeta caches meta.instance_uid into d.instanceUID when
// Migrate is a no-op. The canonical fast-path cacher for handles that won't
// run any DDL.
func (d *Store) ensureInstanceUIDFromMeta(ctx context.Context) error {
	if d.instanceUID != "" {
		return nil
	}
	var v string
	err := d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	d.instanceUID = v
	return nil
}

// ensureInstanceUIDOnConn is Migrate's pre-commit step: read meta.instance_uid
// if present, otherwise generate one and INSERT ... ON CONFLICT DO NOTHING.
// Runs on the pinned connection so it joins the active transaction.
func ensureInstanceUIDOnConn(ctx context.Context, conn *sql.Conn, cached *string) error {
	var v string
	err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if err == nil {
		*cached = v
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES('instance_uid', ?)
		 ON CONFLICT(key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := conn.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	*cached = stored
	return nil
}

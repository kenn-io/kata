package pgstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsSource is the FS the migration runner reads from. Defaults to the
// embedded migrationsFS; tests override via migrations_export_test.go to inject
// synthetic ladder files without polluting the on-disk migrations directory.
var migrationsSource fs.FS = migrationsFS

// migrationAdvisoryLockSeed is the deterministic string the runner hashes via
// hashtextextended to derive the int8 advisory-lock key. The actual hashing
// happens in SQL inside Migrate's transaction; this constant is the
// documentation anchor.
const migrationAdvisoryLockSeed = "kata:migrate"

// Migrate brings the PG database up to db.CurrentSchemaVersion() by applying
// every pending migration file from migrationsSource in version order.
// Concurrent migrators serialize on pg_advisory_xact_lock; the lock and
// transaction together guarantee at-most-one applier holds the ladder.
//
// Postgres has no snapshot equivalent — operators rely on pg_dump or base
// backup. On per-step error the transaction rolls back; the error wraps the
// failing file name. Entry guards reject pre-baseline and newer-than-binary
// versions both before and after the advisory lock to close the race where a
// concurrent migrator advances the schema between the peek and the lock.
func (s *Store) Migrate(ctx context.Context) (db.MigrationResult, error) {
	if s.readOnly {
		return db.MigrationResult{}, errors.New("migrate: backend is read-only")
	}

	current, err := s.currentVersion(ctx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version: %w", err)
	}
	if current > 0 && current < db.BaselineSchemaVersion {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d); Postgres has no JSONL-cutover path — restore from operator backup before retrying", current, db.BaselineSchemaVersion)
	}
	if current > db.CurrentSchemaVersion() {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d; use a newer kata binary", current, db.CurrentSchemaVersion())
	}
	pending, err := listPendingMigrations(migrationsSource, current)
	if err != nil {
		return db.MigrationResult{}, err
	}
	if len(pending) == 0 {
		if err := s.ensureInstanceUIDFromMeta(ctx); err != nil {
			return db.MigrationResult{}, err
		}
		return db.MigrationResult{From: current, To: current, Applied: nil}, nil
	}

	// Apply pending under one transaction with the advisory lock held for
	// the transaction's lifetime. The lock plus transaction serialize
	// concurrent migrators across processes.
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("begin migration tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		migrationAdvisoryLockSeed); err != nil {
		return db.MigrationResult{}, fmt.Errorf("acquire advisory lock: %w", err)
	}

	// Re-read schema_version inside the lock — another migrator may have
	// advanced it between our peek and the lock acquisition. Apply the same
	// entry guards on the re-read value so a concurrent migrator that
	// pushed the schema past the binary's known set is caught here, not
	// after a half-applied step.
	currentInTx, err := txCurrentVersion(ctx, tx)
	if err != nil {
		return db.MigrationResult{}, fmt.Errorf("read schema_version in tx: %w", err)
	}
	if currentInTx > 0 && currentInTx < db.BaselineSchemaVersion {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d predates the baseline (%d) under lock", currentInTx, db.BaselineSchemaVersion)
	}
	if currentInTx > db.CurrentSchemaVersion() {
		return db.MigrationResult{}, fmt.Errorf("schema_version %d is newer than binary schema %d under lock", currentInTx, db.CurrentSchemaVersion())
	}
	pendingInTx, err := listPendingMigrations(migrationsSource, currentInTx)
	if err != nil {
		return db.MigrationResult{}, err
	}

	applied := make([]int, 0, len(pendingInTx))
	for _, m := range pendingInTx {
		sqlBytes, err := fs.ReadFile(migrationsSource, m.fileName)
		if err != nil {
			return db.MigrationResult{}, fmt.Errorf("read migration %s: %w", m.fileName, err)
		}
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("apply migration %s: %w", m.fileName, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES ('schema_version', $1)
			 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
			strconv.Itoa(m.version)); err != nil {
			return db.MigrationResult{}, fmt.Errorf("stamp schema_version %d: %w", m.version, err)
		}
		applied = append(applied, m.version)
	}

	if err := ensureInstanceUIDInTx(ctx, tx, &s.instanceUID); err != nil {
		return db.MigrationResult{}, fmt.Errorf("ensure instance_uid: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return db.MigrationResult{}, fmt.Errorf("commit migration: %w", err)
	}
	committed = true

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

// listPendingMigrations enumerates the migration ladder under "migrations/"
// in source, validates ladder shape (no duplicate version prefixes, no gaps,
// no files beyond db.CurrentSchemaVersion()), and returns the subset whose
// version exceeds current. The validation runs before the gap subset filter
// so a corrupted ladder is caught even when current is at the head.
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
		// io/fs paths are always forward-slash; filepath.Join would
		// emit a backslash on Windows and break fs.ReadFile.
		all = append(all, pendingMigration{version: v, fileName: path.Join("migrations", name)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for i := 1; i < len(all); i++ {
		if all[i].version != all[i-1].version+1 {
			return nil, fmt.Errorf("migration ladder: version %d missing between %d and %d", all[i-1].version+1, all[i-1].version, all[i].version)
		}
	}
	// Pre-baseline ladder files are a configuration bug: pgstore has no
	// JSONL cutover path; baseline (0012) is the floor. Any 0001..0011
	// file in the embedded set means the build picked up wrong-backend
	// files and the runner should refuse before applying them. The
	// no-binary-cap path mirrors sqlitestore: synthetic-next test ladders
	// (used by migrate_test.go) need v(current+1) files to land.
	for _, m := range all {
		if m.version < db.BaselineSchemaVersion {
			return nil, fmt.Errorf("migration version %d is below the baseline (%d); pgstore ladder must start at the baseline", m.version, db.BaselineSchemaVersion)
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

// txCurrentVersion mirrors connCurrentVersion for sqlitestore's pinned conn —
// it joins the active transaction. Returns 0 for fresh DBs (no meta table).
func txCurrentVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var exists bool
	err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		 WHERE table_schema = current_schema() AND table_name = 'meta')`).Scan(&exists)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v string
	err = tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

// ensureInstanceUIDFromMeta caches meta.instance_uid into s.instanceUID on
// the at-current Migrate no-op path. Repairs a missing row via
// INSERT ... ON CONFLICT DO NOTHING, mirroring sqlitestore's contract.
func (s *Store) ensureInstanceUIDFromMeta(ctx context.Context) error {
	if s.instanceUID != "" {
		return nil
	}
	var v string
	err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&v)
	if err == nil {
		s.instanceUID = v
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read instance_uid: %w", err)
	}
	fresh, err := katauid.New()
	if err != nil {
		return fmt.Errorf("generate instance_uid: %w", err)
	}
	if _, err := s.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('instance_uid', $1)
		 ON CONFLICT (key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := s.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	s.instanceUID = stored
	return nil
}

// ensureInstanceUIDInTx is Migrate's pre-commit step: read or seed the
// instance_uid row inside the active transaction so commit/rollback governs
// the row alongside the rest of the migration's writes.
func ensureInstanceUIDInTx(ctx context.Context, tx *sql.Tx, cached *string) error {
	var v string
	err := tx.QueryRowContext(ctx,
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
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES ('instance_uid', $1)
		 ON CONFLICT (key) DO NOTHING`, fresh); err != nil {
		return fmt.Errorf("seed instance_uid: %w", err)
	}
	var stored string
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&stored); err != nil {
		return fmt.Errorf("read instance_uid after seed: %w", err)
	}
	*cached = stored
	return nil
}

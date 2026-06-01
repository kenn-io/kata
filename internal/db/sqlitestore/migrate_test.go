package sqlitestore_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestMigrate_OnAtCurrentDBIsNoop(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	// First Migrate brings the fresh DB to current.
	_, err = d.Migrate(ctx)
	require.NoError(t, err)

	// Second Migrate is the no-op contract under test.
	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
}

// syntheticLadderWithBaseline builds a fstest.MapFS containing every embedded
// migration up to db.CurrentSchemaVersion() plus the caller's additional
// files. Tests use this to stand up a synthetic ladder anchored on the real
// baseline (and post-baseline rungs) so the SQL is guaranteed to apply against
// a fresh DB and so adding a synthetic v(current+N) doesn't fail ladder-gap
// validation when current > baseline.
func syntheticLadderWithBaseline(t *testing.T, extra map[string]*fstest.MapFile) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(sqlitestore.EmbeddedMigrationsFS(), "migrations")
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		key := "migrations/" + e.Name()
		body, err := fs.ReadFile(sqlitestore.EmbeddedMigrationsFS(), key)
		require.NoError(t, err)
		out[key] = &fstest.MapFile{Data: body}
	}
	for name, file := range extra {
		out[name] = file
	}
	return out
}

// migrationFilePath returns the io/fs key for a migration of the given
// version, matching the production naming convention used by the runner.
func migrationFilePath(v int) string {
	suffix := "_test.sql"
	if v == db.BaselineSchemaVersion {
		suffix = "_baseline.sql"
	}
	return fmt.Sprintf("migrations/%04d%s", v, suffix)
}

func TestMigrate_AppliesSyntheticNextVersion(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	next := db.CurrentSchemaVersion() + 1

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	// First bring the DB to the baseline via the embedded ladder.
	_, err = d.Migrate(ctx)
	require.NoError(t, err)

	// Now switch to a synthetic ladder that adds the next-version migration.
	synthetic := syntheticLadderWithBaseline(t, map[string]*fstest.MapFile{
		migrationFilePath(next): {Data: []byte(`
CREATE TABLE migration_marker (
  id INTEGER PRIMARY KEY,
  note TEXT NOT NULL
);
`)},
	})
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, next, result.To)
	assert.Equal(t, []int{next}, result.Applied)

	// The marker table now exists.
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migration_marker'`).Scan(&n))
	assert.Equal(t, 1, n)

	// Snapshot was created and deleted on success: the namespaced path is
	// $KATA_HOME/runtime/<DBHash(dbPath)>/premigrate-v<from>.db.
	expectedSnap := snapshotPath(dbPath, db.CurrentSchemaVersion())
	_, statErr := os.Stat(expectedSnap)
	assert.True(t, os.IsNotExist(statErr), "snapshot should be removed on success; got %v", statErr)
}

// snapshotPath rebuilds the snapshot filename the runner writes when migrating
// from fromVersion. Mirrors Migrate's takeRecoverySnapshot helper.
func snapshotPath(dbPath string, fromVersion int) string {
	return filepath.Join(os.Getenv("KATA_HOME"), "runtime",
		config.DBHash(dbPath),
		fmt.Sprintf("premigrate-v%d.db", fromVersion))
}

func TestMigrate_RollsBackOnBrokenMigration(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	next := db.CurrentSchemaVersion() + 1

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	// First bring the DB to the baseline via the embedded ladder.
	_, err = d.Migrate(ctx)
	require.NoError(t, err)

	// Swap in a ladder whose next-version migration attempts to re-CREATE the
	// meta table — the baseline already created it, so the apply fails inside
	// the transaction and Migrate must roll back.
	synthetic := syntheticLadderWithBaseline(t, map[string]*fstest.MapFile{
		migrationFilePath(next): {Data: []byte(
			`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);`)},
	})
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	// The snapshot path appears in the error so an operator can restore.
	expectedSnap := snapshotPath(dbPath, db.CurrentSchemaVersion())
	assert.Contains(t, err.Error(), expectedSnap)

	// schema_version unchanged.
	v, sverr := d.SchemaVersion(ctx)
	require.NoError(t, sverr)
	assert.Equal(t, db.CurrentSchemaVersion(), v)

	// Snapshot file remains on disk for recovery.
	_, statErr := os.Stat(expectedSnap)
	require.NoError(t, statErr)
}

func TestMigrate_ConcurrentMigratorsSerialize(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	next := db.CurrentSchemaVersion() + 1

	dbPath := filepath.Join(t.TempDir(), "kata.db")
	// First bring the DB to baseline via the embedded ladder, then close.
	bootstrap, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	_, err = bootstrap.Migrate(ctx)
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())

	// Now switch to a synthetic ladder that adds the next-version migration
	// and open two concurrent handles.
	synthetic := syntheticLadderWithBaseline(t, map[string]*fstest.MapFile{
		migrationFilePath(next): {Data: []byte(
			`CREATE TABLE concurrent_marker (id INTEGER PRIMARY KEY);`)},
	})
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	d1, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d1.Close() })
	d2, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	type outcome struct {
		applied []int
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, d := range []*sqlitestore.Store{d1, d2} {
		wg.Add(1)
		go func(store *sqlitestore.Store) {
			defer wg.Done()
			r, err := store.Migrate(ctx)
			results <- outcome{applied: r.Applied, err: err}
		}(d)
	}
	wg.Wait()
	close(results)

	totalApplied := 0
	var failureCount int
	for o := range results {
		if o.err != nil {
			failureCount++
			continue
		}
		totalApplied += len(o.applied)
	}
	// At least one migrator succeeds. The other either also succeeds (with an
	// empty Applied because it observed the post-migration version in the
	// lock) or fails on busy lock contention. The marker is created exactly
	// once.
	assert.LessOrEqual(t, failureCount, 1)
	assert.Equal(t, 1, totalApplied)

	var n int
	require.NoError(t, d1.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='concurrent_marker'`).Scan(&n))
	assert.Equal(t, 1, n)
}

func TestMigrate_RejectsPreBaseline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	// Bring the DB to current, then re-stamp meta with a pre-baseline value.
	_, err = d.Migrate(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='8' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates the baseline")
	assert.Contains(t, err.Error(), "storeopen.Open")
}

func TestMigrate_RejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Migrate(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='99' WHERE key='schema_version'`)
	require.NoError(t, err)

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
}

func TestMigrate_RejectsDuplicateLadderVersions(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	next := db.CurrentSchemaVersion() + 1

	// Validate via a fresh DB; the duplicate-version check fires before any
	// migration applies, so we don't need a baseline to exercise it.
	synthetic := syntheticLadderWithBaseline(t, map[string]*fstest.MapFile{
		fmt.Sprintf("migrations/%04d_a.sql", next): {Data: []byte(`SELECT 1;`)},
		fmt.Sprintf("migrations/%04d_b.sql", next): {Data: []byte(`SELECT 1;`)},
	})
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate files")
}

func TestMigrate_RejectsLadderGaps(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	// The ladder skips next (v+1) and lands at v+2, so the gap check should
	// reject before any migration applies.
	skipTo := db.CurrentSchemaVersion() + 2
	missing := db.CurrentSchemaVersion() + 1

	synthetic := syntheticLadderWithBaseline(t, map[string]*fstest.MapFile{
		migrationFilePath(skipTo): {Data: []byte(`SELECT 1;`)},
	})
	restore := sqlitestore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	_, err = d.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version "+strconv.Itoa(missing)+" missing")
}

// TestMigrate_FreshDBAppliesBaseline confirms that after Task 5 removes
// bootstrap from sqlitestore.Open, the baseline migration is what stamps a
// fresh DB at the current schema version.
func TestMigrate_FreshDBAppliesBaseline(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	result, err := d.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	// Applied lists every rung the runner climbed, baseline plus every
	// post-baseline file. With CurrentSchemaVersion=13 the embedded ladder
	// is [12, 13]; the tail (last value) must equal CurrentSchemaVersion.
	require.NotEmpty(t, result.Applied)
	assert.Equal(t, db.BaselineSchemaVersion, result.Applied[0])
	assert.Equal(t, db.CurrentSchemaVersion(), result.Applied[len(result.Applied)-1])

	// The baseline tables now exist.
	var n int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&n))
	assert.Equal(t, 1, n)

	// The 0013 UNIQUE partial index landed too.
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_events_idempotency_uniq'`).Scan(&n))
	assert.Equal(t, 1, n)

	// instance_uid was stamped.
	assert.NotEmpty(t, d.InstanceUID())
}

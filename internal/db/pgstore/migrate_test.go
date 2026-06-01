package pgstore_test

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// migrationFilePath returns the io/fs key for a synthetic migration of the
// given version. Mirrors the embedded naming convention.
func migrationFilePath(v int) string {
	suffix := "_test.sql"
	if v == db.BaselineSchemaVersion {
		suffix = "_baseline.sql"
	}
	return fmt.Sprintf("migrations/%04d%s", v, suffix)
}

// syntheticLadder builds a MapFS containing every embedded migration plus the
// caller's extra files, so synthetic-next tests inherit the real
// baseline + 0013 rungs and only add their own.
func syntheticLadder(t *testing.T, extra map[string]*fstest.MapFile) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	entries, err := fs.ReadDir(pgstore.EmbeddedMigrationsFS(), "migrations")
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		key := "migrations/" + e.Name()
		body, err := fs.ReadFile(pgstore.EmbeddedMigrationsFS(), key)
		require.NoError(t, err)
		out[key] = &fstest.MapFile{Data: body}
	}
	for name, file := range extra {
		out[name] = file
	}
	return out
}

// TestMigrate_FreshDBAppliesBaseline proves Migrate on a fresh PG instance
// runs the embedded ladder, lands at db.CurrentSchemaVersion, and stamps
// instance_uid into meta.
func TestMigrate_FreshDBAppliesBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	// Applied lists every version that landed; with v12 baseline + v13
	// idempotency this is [12, 13] at the time Phase 3 completes.
	assert.NotEmpty(t, result.Applied)
	assert.Equal(t, db.CurrentSchemaVersion(), result.Applied[len(result.Applied)-1])

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)

	assert.NotEmpty(t, s.InstanceUID())
}

// TestMigrate_AtCurrentIsNoop proves a second Migrate on an already-current
// DB returns the no-op result without applying anything.
func TestMigrate_AtCurrentIsNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
}

// TestMigrate_ReadOnlyReturnsError proves a read-only Store refuses Migrate.
func TestMigrate_ReadOnlyReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
}

// TestMigrate_AppliesSyntheticNextVersion proves the runner advances past
// the embedded current schema when a synthetic v+1 file is in the ladder.
func TestMigrate_AppliesSyntheticNextVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	next := db.CurrentSchemaVersion() + 1

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Bring the DB to current via the embedded ladder.
	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	// Swap in a synthetic ladder with a v+1 marker.
	synthetic := syntheticLadder(t, map[string]*fstest.MapFile{
		migrationFilePath(next): {Data: []byte(`
CREATE TABLE pgstore_migration_marker (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  note TEXT NOT NULL
);
`)},
	})
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	result, err := s.Migrate(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, next, result.To)
	assert.Equal(t, []int{next}, result.Applied)

	var n int
	require.NoError(t, s.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name = 'pgstore_migration_marker'`).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestMigrate_RollsBackOnBrokenMigration proves that a SQL failure inside
// the migration transaction rolls back every step including the
// meta.schema_version stamp.
func TestMigrate_RollsBackOnBrokenMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	next := db.CurrentSchemaVersion() + 1

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	synthetic := syntheticLadder(t, map[string]*fstest.MapFile{
		// Re-CREATE the meta table — it exists; the apply must fail.
		migrationFilePath(next): {Data: []byte(
			`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);`)},
	})
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	_, err = s.Migrate(ctx)
	require.Error(t, err)

	// Schema version unchanged.
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

// TestMigrate_ConcurrentMigratorsSerialize proves the pg_advisory_xact_lock
// serializes concurrent migrators so the synthetic v+1 marker is created
// exactly once.
func TestMigrate_ConcurrentMigratorsSerialize(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	next := db.CurrentSchemaVersion() + 1

	// Bring the DB to current.
	bootstrap, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	_, err = bootstrap.Migrate(ctx)
	require.NoError(t, err)
	require.NoError(t, bootstrap.Close())

	synthetic := syntheticLadder(t, map[string]*fstest.MapFile{
		migrationFilePath(next): {Data: []byte(
			`CREATE TABLE pgstore_concurrent_marker (id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY);`)},
	})
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s1, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s1.Close() })
	s2, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	type outcome struct {
		applied []int
		err     error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, st := range []*pgstore.Store{s1, s2} {
		wg.Add(1)
		go func(store *pgstore.Store) {
			defer wg.Done()
			r, err := store.Migrate(ctx)
			results <- outcome{applied: r.Applied, err: err}
		}(st)
	}
	wg.Wait()
	close(results)

	totalApplied := 0
	failureCount := 0
	for o := range results {
		if o.err != nil {
			// The loser of the lock race can legitimately see the
			// stamped v(current+1) and raise "newer than binary
			// schema under lock". One failure is OK; both is not.
			failureCount++
			continue
		}
		totalApplied += len(o.applied)
	}
	assert.LessOrEqual(t, failureCount, 1, "at most one migrator may fail")
	// At least one migrator applied the synthetic rung.
	assert.GreaterOrEqual(t, totalApplied, 1)

	var n int
	require.NoError(t, s1.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema = current_schema()
		   AND table_name = 'pgstore_concurrent_marker'`).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestMigrate_RejectsPreBaseline proves a DB whose schema_version is below
// the baseline floor is refused — pgstore has no JSONL cutover path.
func TestMigrate_RejectsPreBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	// Re-stamp meta with a pre-baseline value.
	_, err = s.ExecContext(ctx,
		`UPDATE meta SET value = $1 WHERE key='schema_version'`,
		strconv.Itoa(db.BaselineSchemaVersion-1))
	require.NoError(t, err)

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "predates the baseline")
}

// TestMigrate_RejectsNewerThanBinary proves a DB stamped above the binary's
// current version is refused.
func TestMigrate_RejectsNewerThanBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	_, err = s.ExecContext(ctx,
		`UPDATE meta SET value = $1 WHERE key='schema_version'`,
		strconv.Itoa(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
}

// TestMigrate_RejectsDuplicateLadder proves the ladder validator refuses two
// files sharing a version prefix.
func TestMigrate_RejectsDuplicateLadder(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	// Stand up a synthetic ladder with two files at the baseline rung.
	bogus := fmt.Sprintf("migrations/%04d_dup.sql", db.BaselineSchemaVersion)
	synthetic := syntheticLadder(t, map[string]*fstest.MapFile{
		bogus: {Data: []byte(`SELECT 1;`)},
	})
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate files")
}

// TestMigrate_RejectsLadderGaps proves the ladder validator refuses a gap
// between rungs (e.g. v(current+2) without v(current+1)).
func TestMigrate_RejectsLadderGaps(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	skipTo := db.CurrentSchemaVersion() + 2
	missing := db.CurrentSchemaVersion() + 1

	synthetic := syntheticLadder(t, map[string]*fstest.MapFile{
		migrationFilePath(skipTo): {Data: []byte(`SELECT 1;`)},
	})
	restore := pgstore.SetMigrationsSource(synthetic)
	t.Cleanup(restore)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"version "+strconv.Itoa(missing)+" missing")
}

// TestMigrate_InstanceUIDRepairOnAtCurrent proves the at-current no-op
// path repairs a missing instance_uid row via INSERT ON CONFLICT DO NOTHING.
func TestMigrate_InstanceUIDRepairOnAtCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	s, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.Migrate(ctx)
	require.NoError(t, err)

	// Wipe instance_uid out-of-band.
	_, err = s.ExecContext(ctx,
		`DELETE FROM meta WHERE key='instance_uid'`)
	require.NoError(t, err)

	// Reopen to clear the cached instance_uid in the Store.
	require.NoError(t, s.Close())
	s2, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	require.Empty(t, s2.InstanceUID())

	result, err := s2.Migrate(ctx)
	require.NoError(t, err)
	// Already at current — no pending work, the at-current branch ran
	// the repair and stamped a fresh instance_uid.
	assert.Equal(t, db.CurrentSchemaVersion(), result.From)
	assert.Equal(t, db.CurrentSchemaVersion(), result.To)
	assert.Nil(t, result.Applied)
	assert.NotEmpty(t, s2.InstanceUID())
}

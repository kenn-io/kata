package sqlitestore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

func TestOpen_AppliesPragmas(t *testing.T) {
	t.Setenv("KATA_TEST_FAST_SQLITE", "")

	d := openTestDB(t)

	var fk int
	require.NoError(t, d.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk)

	var mode string
	require.NoError(t, d.QueryRow("PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)
}

// TestOpen_OnFreshDBBootstrapsSchema confirms the new Open contract: a fresh
// path returns a handle whose meta table and schema_version row are already
// in place, courtesy of the bootstrap-on-Open transaction.
func TestOpen_OnFreshDBBootstrapsSchema(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n))
	assert.Equal(t, 1, n)

	v, err := d.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

// TestOpen_IsIdempotentAfterBootstrap confirms that re-opening an
// already-bootstrapped DB succeeds and reports the same schema_version and
// instance_uid.
func TestOpen_IsIdempotentAfterBootstrap(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	d1, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	uid1 := d1.InstanceUID()
	require.NoError(t, d1.Close())

	d2, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	v, err := d2.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
	assert.Equal(t, uid1, d2.InstanceUID())
}

func TestOpen_RejectsVersionZeroExistingTables(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='0' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	reopened, err := sqlitestore.Open(ctx, path)
	assert.Nil(t, reopened)
	require.Error(t, err)
	assert.ErrorIs(t, err, sqlitestore.ErrSchemaCutoverRequired)
}

func TestSchema_IssuesHasShortIDColumn(t *testing.T) {
	d := openTestDB(t)
	var typ string
	err := d.QueryRow(
		`SELECT type FROM pragma_table_info('issues') WHERE name='short_id'`,
	).Scan(&typ)
	require.NoError(t, err)
	assert.Equal(t, "TEXT", typ)
}

func TestSchema_IssuesNumberColumnGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('issues') WHERE name='number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_ProjectsNextIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name='next_issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_EventsIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('events') WHERE name='issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_PurgeLogIssueNumberGone(t *testing.T) {
	d := openTestDB(t)
	var n int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('purge_log') WHERE name='issue_number'`,
	).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestSchema_ProjectNameRejectsHash(t *testing.T) {
	d := openTestDB(t)
	_, err := d.Exec(
		`INSERT INTO projects(uid, name) VALUES('01HZNQ7VFPK1XGD8R5MABCD4EX', 'has#hash')`,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHECK")
}

func TestSchemaVersion(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	v, err := d.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), v)
}

func TestOpen_TimestampColumnsScanIntoTime(t *testing.T) {
	d := openTestDB(t)

	projectUID, err := uid.New()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO projects(uid, name) VALUES(?,'x')`, projectUID)
	require.NoError(t, err)

	rows, err := d.Query(`SELECT created_at FROM projects`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next())
	var ts any
	require.NoError(t, rows.Scan(&ts))
	// modernc.org/sqlite returns time.Time for DATETIME columns
	_, ok := ts.(interface{ Year() int })
	assert.True(t, ok, "expected time.Time, got %T", ts)
}

func TestCheckpointTruncatesWAL(t *testing.T) {
	t.Setenv("KATA_TEST_FAST_SQLITE", "")
	t.Setenv("KATA_HOME", t.TempDir())

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	d.SetMaxOpenConns(1)

	_, err = d.ExecContext(ctx, `PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `CREATE TABLE checkpoint_noise(id INTEGER PRIMARY KEY, body BLOB)`)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		WITH RECURSIVE seq(x) AS (
			SELECT 1
			UNION ALL
			SELECT x + 1 FROM seq WHERE x < 128
		)
		INSERT INTO checkpoint_noise(body)
		SELECT randomblob(4096) FROM seq`)
	require.NoError(t, err)

	before, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Positive(t, before.Size(), "test setup must create WAL frames")

	require.NoError(t, d.Checkpoint(ctx))

	after, err := os.Stat(path + "-wal")
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	require.NoError(t, err)
	assert.Zero(t, after.Size(), "TRUNCATE checkpoint should leave no WAL bytes")
}

func TestOpenUsesFastSQLitePragmasWhenTestHarnessRequestsIt(t *testing.T) {
	t.Setenv("KATA_TEST_FAST_SQLITE", "1")
	t.Setenv("KATA_HOME", t.TempDir())

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	var mode string
	require.NoError(t, d.QueryRow("PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)

	var sync int
	require.NoError(t, d.QueryRow("PRAGMA synchronous").Scan(&sync))
	assert.Zero(t, sync)

	var tempStore int
	require.NoError(t, d.QueryRow("PRAGMA temp_store").Scan(&tempStore))
	assert.Equal(t, 2, tempStore)
}

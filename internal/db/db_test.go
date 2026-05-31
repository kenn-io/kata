package db_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
	_ "modernc.org/sqlite"
)

func TestOpen_AppliesPragmasAndMigrations(t *testing.T) {
	d := openTestDB(t)

	var fk int
	require.NoError(t, d.QueryRow("PRAGMA foreign_keys").Scan(&fk))
	assert.Equal(t, 1, fk)

	var mode string
	require.NoError(t, d.QueryRow("PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)

	assertSchemaVersion(t, d, db.CurrentSchemaVersion())
}

func TestOpen_RecordsCurrentSchemaVersion(t *testing.T) {
	assert.Equal(t, 12, db.CurrentSchemaVersion())
	d := openTestDB(t)
	assertSchemaVersion(t, d, db.CurrentSchemaVersion())
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

func TestOpen_IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	d1, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, d1.Close())

	d2, err := db.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	assertSchemaVersion(t, d2, db.CurrentSchemaVersion())
}

func TestOpen_RejectsOlderSchemaNeedingJSONLCutover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO meta(key, value) VALUES('schema_version', '1')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	_, err = db.Open(context.Background(), path)

	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSchemaCutoverRequired), err)
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
	var ts interface{}
	require.NoError(t, rows.Scan(&ts))
	// modernc.org/sqlite returns time.Time for DATETIME columns
	_, ok := ts.(interface{ Year() int })
	assert.True(t, ok, "expected time.Time, got %T", ts)
}

func TestCheckpointTruncatesWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := db.Open(ctx, path)
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

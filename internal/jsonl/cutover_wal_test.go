package jsonl_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestAutoCutoverCheckpointsSourceWALBeforeSwap(t *testing.T) {
	ctx, original := setupClosedTestDB(t)
	source, err := sqlitestore.Open(ctx, original)
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "before WAL commit", Author: "example-actor"})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	writer, err := sql.Open("sqlite", original)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = writer.Close() })
	_, err = writer.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
 UPDATE meta SET value='28' WHERE key='schema_version';
 UPDATE issues SET title='committed in source WAL' WHERE id=?`, issue.ID)
	require.NoError(t, err)

	// Copy a stable committed database+WAL pair while its writer stays open.
	// This is a crash image: no graceful-close checkpoint touches the copy.
	path := filepath.Join(t.TempDir(), "kata.db")
	for _, suffix := range []string{"", "-wal"} {
		body, err := os.ReadFile(original + suffix) //nolint:gosec // source and WAL files are created under this test's temporary database directory.
		require.NoError(t, err)
		require.NotEmpty(t, body)
		require.NoError(t, os.WriteFile(path+suffix, body, 0o600)) //nolint:gosec // crash-image files are copied under t.TempDir
	}
	version, err := sqlitestore.PeekSchemaVersion(ctx, path)
	require.NoError(t, err)
	require.Equal(t, 28, version)
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	// Check sidecars before any read-only reopen: even a read-only connection
	// can create fresh empty WAL/SHM files for the replacement's own layout.
	for _, suffix := range []string{"-wal", "-shm"} {
		require.NoFileExists(t, path+suffix)
	}
	assertCurrentSchemaVersion(t, path)
	target, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	var title string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=?`, issue.UID).Scan(&title))
	require.Equal(t, "committed in source WAL", title)
	var integrity string
	require.NoError(t, target.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity))
	require.Equal(t, "ok", integrity)
	backups, err := filepath.Glob(path + ".bak.*")
	require.NoError(t, err)
	require.Len(t, backups, 1)
	backup, err := sql.Open("sqlite", "file:"+backups[0]+"?mode=ro")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backup.Close()) })
	require.NoError(t, backup.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=?`, issue.UID).Scan(&title))
	require.Equal(t, "committed in source WAL", title, "backup must include committed WAL state")
}

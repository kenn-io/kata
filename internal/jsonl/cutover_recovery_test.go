package jsonl_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestAutoCutoverRecoversInterruptedImport(t *testing.T) {
	ctx, path := setupClosedTestDB(t)
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Preserve this issue", Author: "example-actor"})
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	suffixes := []string{".import.tmp.jsonl", ".import.tmp.db", ".import.tmp.db-wal", ".import.tmp.db-shm"}
	for _, suffix := range suffixes {
		require.NoError(t, os.WriteFile(path+suffix, []byte("partial "+suffix), 0o600))
	}
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	assertCurrentSchemaVersion(t, path)
	assertNoCutoverTemps(t, path)
	dirs, err := filepath.Glob(path + ".import.recovery-*")
	require.NoError(t, err)
	require.Len(t, dirs, 1)
	for _, suffix := range suffixes {
		body, err := os.ReadFile(filepath.Join(dirs[0], filepath.Base(path)+suffix)) //nolint:gosec // recovery artifacts are created under this test's temporary database directory.
		require.NoError(t, err)
		require.Equal(t, "partial "+suffix, string(body))
	}
	target, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	var title string
	require.NoError(t, target.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=?`, issue.UID).Scan(&title))
	require.Equal(t, "Preserve this issue", title)
}

func TestAutoCutoverRefusesConcurrentCutover(t *testing.T) {
	ctx, path := setupClosedTestDB(t)
	lock := flock.New(path + ".import.lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	err := jsonl.AutoCutover(ctx, path)
	require.ErrorIs(t, err, jsonl.ErrCutoverInProgress)
}

func TestAutoCutoverRetriesTransientReadinessProbe(t *testing.T) {
	ctx, path := setupClosedTestDB(t)
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = source.ExecContext(ctx, `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	lock := flock.New(path + ".import.lock")
	require.NoError(t, lock.Lock())
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	synctest.Test(t, func(t *testing.T) {
		result := make(chan error, 1)
		go func() { result <- jsonl.AutoCutover(ctx, path) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("readiness probe aborted cutover: %v", err)
		default:
		}
		require.NoError(t, lock.Unlock())
		require.NoError(t, <-result)
	})
	assertCurrentSchemaVersion(t, path)
}

func TestAutoCutoverMissingSourcePreservesTemps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	require.NoError(t, os.WriteFile(path+".import.tmp.jsonl", []byte("partial"), 0o600))
	err := jsonl.AutoCutover(t.Context(), path)
	require.ErrorContains(t, err, "source database is unavailable")
	require.NoFileExists(t, path)
	body, err := os.ReadFile(path + ".import.tmp.jsonl") //nolint:gosec // recovery artifact is created under this test's temporary database directory.
	require.NoError(t, err)
	require.Equal(t, "partial", string(body))
}

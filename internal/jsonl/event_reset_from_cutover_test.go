package jsonl_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestEventResetFromIndexCutoverUpgradesVersion43(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "old.db")
	store, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Retained history", Author: "worker"})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DROP INDEX IF EXISTS idx_events_reset_from; UPDATE meta SET value='43' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	events, err := upgraded.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, created.UID, events[0].UID)
	var name string
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND name='idx_events_reset_from'`).Scan(&name))
}

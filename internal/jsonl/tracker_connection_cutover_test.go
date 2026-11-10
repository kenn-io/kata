package jsonl_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func TestTrackerConnectionSQLiteCutoverFromSchema50(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	_, err = source.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `ALTER TABLE issue_sync_bindings DROP COLUMN connection_uid`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `UPDATE meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	target, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	binding, err := target.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Nil(t, binding.ConnectionUID)
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(binding.ConnectionUID, binding.Config))
}

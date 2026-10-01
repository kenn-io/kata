package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Status-only settings do not change what content imports fetch or produce, so
// changing them keeps the content cursor. Other config changes still reset it.
func checkIssueStatusSettingsKeepContentCursor(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	upsert := func(config string) db.IssueSyncBinding {
		binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(config), IntervalSeconds: 60})
		require.NoError(t, err)
		return binding
	}
	binding := upsert(`{"complete_group_id":"g-done"}`)
	setCursor := func() {
		_, err := sqlStore.ExecContext(ctx, `UPDATE issue_sync_bindings SET last_cursor_at=$1 WHERE id=$2`, "2026-09-29T12:00:00.000Z", binding.ID)
		require.NoError(t, err)
	}
	setCursor()
	for _, config := range []string{
		`{"complete_group_id":"g-done","status_sync":"one-way"}`,
		`{"complete_group_id":"g-done","status_sync":"two-way","todo_group_id":"g-todo"}`,
		`{"complete_group_id":"g-done","status_sync":"two-way","todo_group_id":"g-todo","closed_status_id":"done","open_status_id":"ready","closed_state_id":"s-done","open_state_id":"s-open"}`,
	} {
		binding = upsert(config)
		require.NotNil(t, binding.LastCursorAt, "status settings keep the content cursor: %s", config)
	}
	binding = upsert(`{"complete_group_id":"g-other","status_sync":"two-way","todo_group_id":"g-todo"}`)
	require.Nil(t, binding.LastCursorAt, "completion classification is content config")
	setCursor()
	binding = upsert(`{"complete_group_id":"g-other","status_sync":"two-way","todo_group_id":"g-todo","title_prefix":false}`)
	require.Nil(t, binding.LastCursorAt, "content settings reset the cursor")
	return nil
}

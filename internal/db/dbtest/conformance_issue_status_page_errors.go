package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A mapping whose private state cannot be loaded is reported on its own, so
// the scan can record it and still reach later mappings.
func checkIssueStatusPageIsolatesInvalidMapping(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	reader := store.(db.IssueStatusReader)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: now}
	var ids []int64
	for range 2 {
		issue, err := createFixtureIssue(ctx, store, project.ID, "Mapped task", "worker", nil)
		require.NoError(t, err)
		m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ExternalID: issue.UID, ObjectType: "issue", IssueID: &issue.ID})
		require.NoError(t, err)
		ids = append(ids, m.ID)
	}
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1 WHERE id=$2`, "01HZZZZZZZZZZZZZZZZZZZZZ99", ids[0])
	require.NoError(t, err)

	for _, pending := range []bool{false, true} {
		page, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 100, PendingOnly: pending})
		require.NoError(t, err, "one invalid mapping must not fail the page")
		require.NotEmpty(t, page.Mappings)
		require.Equal(t, ids[0], page.Mappings[0].Mapping.ID)
		require.ErrorIs(t, page.Mappings[0].LoadError, db.ErrImportValidation)
		if !pending {
			require.Len(t, page.Mappings, 2)
			require.Equal(t, ids[1], page.Mappings[1].Mapping.ID)
			require.NoError(t, page.Mappings[1].LoadError)
		}
	}
	return nil
}

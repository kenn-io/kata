package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A moved issue leaves its close/reopen events in the source project, so the
// mapping must not keep a pending event or observation from the old binding.
func checkIssueStatusMove(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	sqlStore := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	fixture, err := createIssueFixture(ctx, store, "example-source-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "example-target-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: fixture.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: fixture.Project.ID, Source: binding.SourceKey, ExternalID: "page-1", ObjectType: "issue", IssueID: &fixture.Issue.ID})
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status=$1, observed_status_at=$2, remote_locator=$3 WHERE id=$4`, "status-option", "2026-09-29T12:00:00Z", "remote-17", mapping.ID)
	require.NoError(t, err)
	closed, _, changed, err := store.CloseIssueWithEvents(ctx, fixture.Issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	require.True(t, changed)

	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: fixture.Issue.ID, FromProjectID: fixture.Project.ID, ToProjectID: target.ID, IfMatchRev: closed.Revision, Actor: "mover"})
	require.NoError(t, err)

	var raw, observedAt, pending, locator *string
	require.NoError(t, sqlStore.QueryRowContext(ctx, `SELECT observed_status, CAST(observed_status_at AS TEXT), pending_event_uid, remote_locator FROM import_mappings WHERE id=$1`, mapping.ID).Scan(&raw, &observedAt, &pending, &locator))
	require.Nil(t, raw, "a moved mapping drops the old binding's observation")
	require.Nil(t, observedAt)
	require.Nil(t, pending, "a moved mapping drops intent whose event stays in the source project")
	require.Nil(t, locator)

	var records []db.ImportRecord
	for record, err := range store.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportIssues(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportEvents(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	for record, err := range store.ExportImportMappings(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		records = append(records, &record)
	}
	require.NoError(t, db.ValidateImportRecords(records), "export after a move must remain restorable")
	return nil
}

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

func checkIssueStatusMappingReads(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	reader, ok := store.(db.IssueStatusReader)
	require.True(t, ok, "backend must offer claim-fenced private status reads")
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: now}
	ids := make([]int64, 0, 3)
	for _, source := range []string{binding.SourceKey, binding.SourceKey, "other-source"} {
		issue, err := createFixtureIssue(ctx, store, project.ID, "Mapped task", "worker", nil)
		require.NoError(t, err)
		m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: source, ExternalID: issue.UID, ObjectType: "issue", IssueID: &issue.ID})
		require.NoError(t, err)
		ids = append(ids, m.ID)
	}
	for _, limit := range []int{-1, 0, 101} {
		_, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: limit})
		require.ErrorIs(t, err, db.ErrImportValidation)
	}
	first, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Mappings, 1)
	require.Equal(t, ids[0], first.Mappings[0].Mapping.ID)
	require.Equal(t, ids[1], first.HighWaterID, "lap high-watermark excludes other source")
	require.Nil(t, first.Mappings[0].State.Observed)
	require.Empty(t, first.Mappings[0].State.RemoteLocator)
	require.Nil(t, first.Mappings[0].PendingEvent)
	issue, err := createFixtureIssue(ctx, store, project.ID, "Late task", "worker", nil)
	require.NoError(t, err)
	late, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ExternalID: issue.UID, ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	rest, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, AfterID: ids[0], ThroughID: first.HighWaterID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, rest.Mappings, 1)
	require.Equal(t, ids[1], rest.Mappings[0].Mapping.ID)
	require.NotEqual(t, late.ID, rest.Mappings[0].Mapping.ID)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET observed_status_at=$1, remote_locator=$2 WHERE id=$3`, "2026-09-29T12:00:00Z", "remote-17", ids[0])
	require.NoError(t, err)
	current, err := reader.IssueStatusMappingByID(ctx, guard, ids[0])
	require.NoError(t, err)
	require.NotNil(t, current.State.Observed)
	require.Nil(t, current.State.Observed.Raw)
	require.Equal(t, now, current.State.Observed.Version)
	require.Equal(t, "remote-17", current.State.RemoteLocator)
	_, err = reader.IssueStatusMappingByID(ctx, guard, ids[2])
	require.ErrorIs(t, err, db.ErrNotFound)
	pending, err := reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 100, PendingOnly: true})
	require.NoError(t, err)
	require.Empty(t, pending.Mappings)
	_, events, changed, err := store.CloseIssueWithEvents(ctx, *current.Mapping.IssueID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotEmpty(t, events)
	state := events[0].UID
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1 WHERE id=$2`, state, ids[0])
	require.NoError(t, err)
	pending, err = reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: guard, Limit: 100, PendingOnly: true})
	require.NoError(t, err)
	require.Len(t, pending.Mappings, 1)
	require.NotNil(t, pending.Mappings[0].PendingEvent)
	require.Equal(t, events[0].UID, pending.Mappings[0].PendingEvent.UID)
	_, otherEvents, changed, err := store.CloseIssueWithEvents(ctx, issue.ID, "done", "worker", "Completed another task", nil)
	require.NoError(t, err)
	require.True(t, changed)
	foreign := otherEvents[0].UID
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1 WHERE id=$2`, foreign, ids[0])
	require.NoError(t, err)
	_, err = reader.IssueStatusMappingByID(ctx, guard, ids[0])
	require.ErrorIs(t, err, db.ErrImportValidation)
	_, err = sqlStore.ExecContext(ctx, `UPDATE import_mappings SET pending_event_uid=$1 WHERE id=$2`, state, ids[0])
	require.NoError(t, err)
	wrong := guard
	wrong.Provider = "github"
	_, err = reader.IssueStatusMappingByID(ctx, wrong, ids[0])
	require.ErrorIs(t, err, db.ErrIssueSyncNotEnabled)
	stale := guard
	stale.StartedAt = now.Add(time.Second)
	_, err = reader.IssueStatusMappingByID(ctx, stale, ids[0])
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	_, err = reader.ListIssueStatusMappings(ctx, db.IssueStatusQuery{Guard: stale, Limit: 100})
	require.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	_, err = store.DisableIssueSyncBinding(ctx, project.ID)
	require.NoError(t, err)
	_, err = reader.IssueStatusMappingByID(ctx, guard, ids[0])
	require.ErrorIs(t, err, db.ErrIssueSyncNotEnabled)
	return nil
}

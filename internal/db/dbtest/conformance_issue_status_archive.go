package dbtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusArchiveFence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	f, err := createIssueFixture(ctx, store, "example-project", "Mapped task", "worker", nil)
	require.NoError(t, err)
	params := db.UpsertIssueSyncBindingParams{ProjectID: f.Project.ID, Provider: "notion", SourceKey: "notion:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 60}
	b, err := store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	m, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: f.Project.ID, Source: b.SourceKey, ObjectType: "issue", ExternalID: "page:example-id", IssueID: &f.Issue.ID})
	require.NoError(t, err)
	_, event, _, err := store.CloseIssue(ctx, f.Issue.ID, "done", "worker", "Completed task", nil)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	future := at.Add(time.Hour)
	sqlStore := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = sqlStore.ExecContext(ctx, `UPDATE issue_sync_bindings SET updated_at=$1 WHERE id=$2`, future.Format(time.RFC3339Nano), b.ID)
	require.NoError(t, err)
	_, ok, err := store.ClaimIssueSyncBinding(ctx, b.ID, b.Provider, at, at.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, ok)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: f.Project.ID, Actor: "worker"})
	require.NoError(t, err)
	archived, err := store.IssueSyncBindingByID(ctx, b.ID)
	require.NoError(t, err)
	require.True(t, archived.UpdatedAt.After(future), "archiving must advance the existing monotonic admission fence even behind wall time")
	status, err := store.IssueSyncStatusByProject(ctx, b.ProjectID)
	require.NoError(t, err)
	require.Equal(t, &at, status.SyncStartedAt)
	_, _, _, err = store.RestoreProject(ctx, f.Project.ID, "worker")
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	guard := db.IssueSyncImportGuard{BindingID: b.ID, Provider: b.Provider, StartedAt: at, BindingUpdatedAt: &future}
	_, err = store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, m.ID)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	guard.BindingUpdatedAt = nil
	current, err := store.(db.IssueStatusReader).IssueStatusMappingByID(ctx, guard, m.ID)
	require.NoError(t, err)
	require.Equal(t, event.UID, current.State.PendingEventUID)
	return nil
}

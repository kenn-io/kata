package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueStatusContentWarning(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "plane", SourceKey: "plane:example-source", RemoteID: "example-source", DisplayName: "Example tasks", Config: jsontext.Value(`{"status_sync":"two-way"}`), IntervalSeconds: 60})
	require.NoError(t, err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var previousSuccess *time.Time
	previousCount := 0
	for i, warning := range []string{"status write blocked: permission denied", "", "status write blocked: target missing"} {
		started := base.Add(time.Duration(i) * time.Hour)
		_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "plane", started, started.Add(-time.Minute))
		require.NoError(t, err)
		require.True(t, claimed)
		completed := started.Add(time.Minute)
		status, err := store.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{BindingID: binding.ID, StartedAt: started, At: completed, CursorAt: started, StatusError: warning, LastCreated: i + 1, LastUpdated: i + 1, LastUnchanged: i + 1, LastComments: i + 1})
		require.NoError(t, err)
		require.Nil(t, status.SyncStartedAt)
		require.Equal(t, warning, status.LastError)
		if warning == "" {
			previousSuccess = &completed
			previousCount = i + 1
			require.Nil(t, status.LastErrorAt)
		} else {
			require.Equal(t, &completed, status.LastErrorAt)
		}
		require.Equal(t, previousSuccess, status.LastSuccessAt)
		require.Equal(t, previousCount, status.LastCreated)
		require.Equal(t, previousCount, status.LastUpdated, "counters describe the last fully successful run")
		require.Equal(t, previousCount, status.LastUnchanged)
		require.Equal(t, previousCount, status.LastComments)
		persisted, err := store.IssueSyncStatusByProject(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, status, persisted)
		refreshed, err := store.IssueSyncBindingByID(ctx, binding.ID)
		require.NoError(t, err)
		require.Equal(t, &started, refreshed.LastCursorAt, "blocked status does not stall the content cursor")
	}
	return nil
}

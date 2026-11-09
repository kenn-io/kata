package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A successful run must return the exact committed revision for activation.
func checkTrackerRunCommittedRevision(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-revision-project")
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300})
	require.NoError(t, err)
	started := time.Now().UTC().Truncate(time.Millisecond)
	claimCtx := db.WithAdditionalTransactionFence(ctx, db.IssueSyncBindingFence(binding))
	claimedBinding, claimed, err := store.ClaimIssueSyncBinding(claimCtx, binding.ID, binding.Provider, started, started.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, binding.ID, claimedBinding.ID)
	require.True(t, binding.UpdatedAt.Equal(claimedBinding.UpdatedAt))
	status, err := store.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{BindingID: binding.ID, BindingUpdatedAt: &binding.UpdatedAt, StartedAt: started, At: started, CursorAt: started})
	require.NoError(t, err)
	current, err := store.IssueSyncBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	require.True(t, status.CommittedBindingUpdatedAt.Equal(current.UpdatedAt))
	require.True(t, current.UpdatedAt.After(binding.UpdatedAt))
	disableCtx := db.WithAdditionalTransactionFence(ctx, db.IssueSyncBindingFence(current))
	disabled, err := store.DisableIssueSyncBinding(disableCtx, current.ProjectID)
	require.NoError(t, err)
	require.False(t, disabled.Enabled)
	return nil
}

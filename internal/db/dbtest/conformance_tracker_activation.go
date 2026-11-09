package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkTrackerActivation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-activation-project")
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	activation := store.(db.TrackerActivationStore)
	_, found, err := activation.LoadTrackerActivation(ctx, project.UID)
	require.NoError(t, err)
	require.False(t, found)
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	_, err = activation.ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt.Add(-time.Second), epoch)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	active, err := activation.ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, epoch)
	require.NoError(t, err)
	require.True(t, active.Active)
	require.True(t, active.Allows(epoch, binding.ID, selection, binding.SourceKey, active.EventFloor+1))
	require.False(t, active.Allows(epoch, binding.ID, selection, binding.SourceKey, active.EventFloor))
	loaded, found, err := activation.LoadTrackerActivation(ctx, project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, active, loaded)
	return nil
}

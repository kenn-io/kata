package dbtest

import (
	"encoding/json/jsontext"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
	"time"
)

func checkTrackerBindingSelection(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-binding-project")
	require.NoError(t, err)
	selection := db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}
	params := db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: jsontext.Value(`{}`), IntervalSeconds: 300, Connection: &selection}
	named, err := store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	require.NotNil(t, named.ConnectionUID)
	require.Equal(t, selection.ConnectionUID, *named.ConnectionUID)
	got, err := db.IssueSyncConnectionFromConfig(named.Config)
	require.NoError(t, err)
	require.Equal(t, selection, *got)
	params.Connection = nil
	updated, err := store.UpsertIssueSyncBinding(ctx, params)
	require.NoError(t, err)
	require.Equal(t, named.ConnectionUID, updated.ConnectionUID)
	require.NoError(t, db.ValidateIssueSyncConnectionMarker(updated.ConnectionUID, updated.Config))
	next := selection
	next.Generation = "second"
	params.Connection = &next
	params.ExpectedBinding = &db.IssueSyncBindingPrecondition{ID: updated.ID, Config: updated.Config, IntervalSeconds: updated.IntervalSeconds}
	rebind := db.TrackerRebindParams{Binding: params, ExpectedUpdatedAt: updated.UpdatedAt.Add(time.Nanosecond), ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0"}
	rebinder := store.(db.TrackerRebindStore)
	_, err = rebinder.RebindTrackerConnection(ctx, rebind)
	require.ErrorIs(t, err, db.ErrIssueSyncBindingChanged)
	rebind.ExpectedUpdatedAt = updated.UpdatedAt
	rebound, err := rebinder.RebindTrackerConnection(ctx, rebind)
	require.NoError(t, err)
	got, err = db.IssueSyncConnectionFromConfig(rebound.Config)
	require.NoError(t, err)
	require.Equal(t, next, *got)
	require.Equal(t, named.ID, rebound.ID)
	require.Equal(t, named.SourceKey, rebound.SourceKey)
	a, found, err := store.(db.TrackerActivationStore).LoadTrackerActivation(ctx, project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, a.Active, "new account/generation never inherits old activation")
	fence := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Connection: next, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", SourceKey: rebound.SourceKey, Locator: "example-item", Target: "closed", ProcessEpoch: rebind.ProcessEpoch, Phase: db.TrackerWritePrepared}
	fence.WriteUID, err = db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	fences := store.(db.TrackerFenceStore)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, nil, fence, rebind.ProcessEpoch))
	unknown := fence
	unknown.Phase = db.TrackerWriteUnknown
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &fence, unknown, rebind.ProcessEpoch))
	successor := next
	successor.Generation = "third"
	rebind.Binding.Connection = &successor
	rebind.Binding.ExpectedBinding = &db.IssueSyncBindingPrecondition{ID: rebound.ID, Config: rebound.Config, IntervalSeconds: rebound.IntervalSeconds}
	rebind.ExpectedUpdatedAt = rebound.UpdatedAt
	_, err = rebinder.RebindTrackerConnection(ctx, rebind)
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown, "another account/generation cannot escape an old external effect")
	retained, found, err := fences.LoadTrackerExecutionFence(ctx, store.InstanceUID(), project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, unknown, retained)
	for row, err := range store.ExportIssueSyncBindings(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		require.NoError(t, db.ValidateIssueSyncConnectionMarker(row.ConnectionUID, row.Config))
	}
	return nil
}

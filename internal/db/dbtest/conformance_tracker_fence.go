package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Both native backends must retain the same conditional execution head in their
// existing meta store. This is a behavioral fixture, not a DDL-content assertion.
func checkTrackerExecutionFence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	fences, ok := store.(db.TrackerFenceStore)
	require.True(t, ok)
	fence := db.TrackerExecutionFence{
		Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID,
		Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"},
		Sequence:   1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:example-source", Locator: "example-item", Target: "closed",
		ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared,
	}
	fence.WriteUID, err = db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, nil, fence, fence.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, nil, fence, fence.ProcessEpoch), db.ErrTrackerFenceChanged)
	loaded, found, err := fences.LoadTrackerExecutionFence(ctx, fence.RootInstanceUID, project.UID, fence.Role)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, fence, loaded)
	dispatching := fence
	dispatching.Phase = db.TrackerWriteDispatching
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, "01ARZ3NDEKTSV4RRFFQ69G5FB1"), db.ErrTrackerOutcomeUnknown)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, fence.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &fence, dispatching, fence.ProcessEpoch), db.ErrTrackerFenceChanged)
	unknown := dispatching
	unknown.Phase = db.TrackerWriteUnknown
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &dispatching, unknown, fence.ProcessEpoch))
	confirmed := unknown
	confirmed.Phase = db.TrackerWriteConfirmed
	confirmed.ProviderAcknowledged = true
	confirmed.Result = jsontext.Value(`{"status":"closed"}`)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &unknown, confirmed, fence.ProcessEpoch))
	acknowledged := confirmed
	acknowledged.Phase = db.TrackerWriteAcknowledged
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &confirmed, acknowledged, fence.ProcessEpoch))
	next := fence
	next.Sequence = 2
	next.PreviousHead = db.TrackerFenceHead(acknowledged)
	next.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	next.WriteUID, err = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &acknowledged, next, next.ProcessEpoch))
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &acknowledged, next, next.ProcessEpoch), db.ErrTrackerFenceChanged, "an older root head cannot overwrite newer state")
	notSent := next
	notSent.Phase = db.TrackerWriteNotSent
	notSent.Result = jsontext.Value(`{"outcome":"not_sent"}`)
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &next, notSent, "01ARZ3NDEKTSV4RRFFQ69G5FB2"), db.ErrTrackerOutcomeUnknown)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &next, notSent, next.ProcessEpoch))
	denied := db.WithAuthorizedProjects(ctx, []string{})
	_, _, err = fences.LoadTrackerExecutionFence(denied, fence.RootInstanceUID, project.UID, fence.Role)
	require.Error(t, err)
	return nil
}

// Contract: an acknowledged semantic event cannot be admitted again after an
// intervening event, even with a fresh sequence and connection generation.
func checkTrackerExecutionHistory(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "tracker-history-project")
	require.NoError(t, err)
	fences := store.(db.TrackerFenceStore)
	first := db.TrackerExecutionFence{Version: 1, Role: "coordinator", RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Connection: db.IssueSyncConnection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Sequence: 1, PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ", SourceKey: "notion:source", Locator: "item", Target: "closed", ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared}
	first.WriteUID, err = db.TrackerStatusWriteUID(first.RootInstanceUID, first.ProjectUID, first.SourceKey, first.Locator, first.PendingEventUID, first.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, nil, first, first.ProcessEpoch))
	complete := func(f db.TrackerExecutionFence) db.TrackerExecutionFence {
		dispatch := f
		dispatch.Phase = db.TrackerWriteDispatching
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &f, dispatch, f.ProcessEpoch))
		confirmed := dispatch
		confirmed.Phase = db.TrackerWriteConfirmed
		confirmed.ProviderAcknowledged = true
		confirmed.Result = jsontext.Value(`{"status":"closed"}`)
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &dispatch, confirmed, f.ProcessEpoch))
		ack := confirmed
		ack.Phase = db.TrackerWriteAcknowledged
		require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &confirmed, ack, f.ProcessEpoch))
		return ack
	}
	ack1 := complete(first)
	second := first
	second.Sequence = 2
	second.PreviousHead = db.TrackerFenceHead(ack1)
	second.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	second.WriteUID, err = db.TrackerStatusWriteUID(second.RootInstanceUID, second.ProjectUID, second.SourceKey, second.Locator, second.PendingEventUID, second.Target)
	require.NoError(t, err)
	require.NoError(t, fences.StoreTrackerExecutionFence(ctx, &ack1, second, second.ProcessEpoch))
	ack2 := complete(second)
	replay := first
	replay.Sequence = 3
	replay.PreviousHead = db.TrackerFenceHead(ack2)
	replay.Connection.Generation = "another"
	require.ErrorIs(t, fences.StoreTrackerExecutionFence(ctx, &ack2, replay, replay.ProcessEpoch), db.ErrTrackerOutcomeUnknown)
	return nil
}

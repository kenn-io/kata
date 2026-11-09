package db_test

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func trackerFenceFixture() db.TrackerExecutionFence {
	f := db.TrackerExecutionFence{
		Version: 1, Role: "executor",
		RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		ProjectUID:      "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Connection:      db.IssueSyncConnection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAX", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"},
		Sequence:        1, WriteUID: fmt.Sprintf("%064x", 1),
		PendingEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAZ",
		SourceKey:       "notion:example-source", Locator: "example-item", Target: "closed",
		ProcessEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Phase: db.TrackerWritePrepared,
	}
	f.WriteUID, _ = db.TrackerStatusWriteUID(f.RootInstanceUID, f.ProjectUID, f.SourceKey, f.Locator, f.PendingEventUID, f.Target)
	return f
}

// Contract: neither time passing nor a new caller/run may reopen an unknown
// provider attempt. Only a definitive provider acknowledgement can unblock it.
func TestTrackerFenceUnknownWriteCannotAdvance(t *testing.T) {
	previous := trackerFenceFixture()
	previous.Phase = db.TrackerWriteUnknown
	next := previous
	next.Sequence++
	next.PreviousHead = db.TrackerFenceHead(previous)
	next.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	next.WriteUID, _ = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
	next.Phase = db.TrackerWritePrepared
	require.ErrorIs(t, db.ValidateTrackerFenceTransition(previous, next), db.ErrTrackerOutcomeUnknown)
	for _, phase := range []db.TrackerWritePhase{db.TrackerWritePrepared, db.TrackerWriteDispatching, db.TrackerWriteAcknowledged} {
		next = previous
		next.Phase = phase
		if phase == db.TrackerWriteAcknowledged {
			next.ProviderAcknowledged = true
			next.Result = jsontext.Value(`{"status":"closed"}`)
		}
		require.NoError(t, next.Validate())
		require.Error(t, db.ValidateTrackerFenceTransition(previous, next))
	}
}

func TestTrackerFenceDefiniteAcknowledgementAllowsOnlyNextHead(t *testing.T) {
	previous := trackerFenceFixture()
	previous.Phase = db.TrackerWriteAcknowledged
	previous.ProviderAcknowledged = true
	previous.Result = jsontext.Value(`{"status":"closed"}`)
	next := trackerFenceFixture()
	next.Sequence = 2
	next.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	next.WriteUID, _ = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
	next.PreviousHead = db.TrackerFenceHead(previous)
	require.NoError(t, db.ValidateTrackerFenceTransition(previous, next))
	for _, mutate := range []func(*db.TrackerExecutionFence){
		func(f *db.TrackerExecutionFence) { f.Sequence = 1; f.PreviousHead = "" },
		func(f *db.TrackerExecutionFence) { f.Sequence = 3 },
		func(f *db.TrackerExecutionFence) { f.PreviousHead = fmt.Sprintf("%064x", 99) },
		func(f *db.TrackerExecutionFence) {
			f.PendingEventUID = previous.PendingEventUID
			f.WriteUID = previous.WriteUID
		},
	} {
		invalid := next
		mutate(&invalid)
		require.NoError(t, invalid.Validate())
		require.Error(t, db.ValidateTrackerFenceTransition(previous, invalid))
	}
}

func TestTrackerFenceConvergenceDoesNotProveProviderAcknowledgement(t *testing.T) {
	previous := trackerFenceFixture()
	previous.Phase = db.TrackerWriteUnknown
	next := previous
	next.Phase = db.TrackerWriteConvergedUnknown
	next.Result = jsontext.Value(`{"status":"closed"}`)
	require.NoError(t, db.ValidateTrackerFenceTransition(previous, next))
	confirmed := next
	confirmed.Phase = db.TrackerWriteConfirmed
	require.Error(t, db.ValidateTrackerFenceTransition(next, confirmed))
	confirmed.ProviderAcknowledged = true
	require.NoError(t, db.ValidateTrackerFenceTransition(next, confirmed))
}

func TestTrackerFenceRestartCannotUseOldPreparedPermit(t *testing.T) {
	fence := trackerFenceFixture()
	require.True(t, fence.CanDispatch(fence.ProcessEpoch))
	require.False(t, fence.CanDispatch("01ARZ3NDEKTSV4RRFFQ69G5FB1"))
	fence.ActivationRequired = true
	require.False(t, fence.CanDispatch(fence.ProcessEpoch))
	fence.ActivationRequired = false
	fence.Phase = db.TrackerWriteDispatching
	require.False(t, fence.CanDispatch(fence.ProcessEpoch), "dispatching is never permission to repeat the call")
}

func TestTrackerFenceRejectsCallerChosenWriteIdentity(t *testing.T) {
	fence := trackerFenceFixture()
	fence.WriteUID = fmt.Sprintf("%064x", 999)
	require.ErrorIs(t, fence.Validate(), db.ErrTrackerFenceInvalid)
}

func TestTrackerWriteIdentityDoesNotDependOnCallerOrConnection(t *testing.T) {
	fence := trackerFenceFixture()
	first, err := db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	fence.Connection.Generation = "second"
	fence.Sequence = 123
	fence.ProcessEpoch = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	second, err := db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator, fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	require.Equal(t, first, second)
	other, err := db.TrackerStatusWriteUID(fence.RootInstanceUID, fence.ProjectUID, fence.SourceKey, fence.Locator+"-other", fence.PendingEventUID, fence.Target)
	require.NoError(t, err)
	require.NotEqual(t, first, other)
}

func TestTrackerProvenUnsentRetirementDoesNotClaimAcknowledgement(t *testing.T) {
	prepared := trackerFenceFixture()
	retired := prepared
	retired.Phase = db.TrackerWriteNotSent
	retired.Result = []byte(`{"outcome":"not_sent"}`)
	require.NoError(t, db.ValidateTrackerFenceTransition(prepared, retired))
	require.False(t, retired.ProviderAcknowledged)
	require.False(t, retired.CanDispatch(retired.ProcessEpoch))
	next := prepared
	next.Sequence = 2
	next.PreviousHead = db.TrackerFenceHead(retired)
	next.PendingEventUID = "01ARZ3NDEKTSV4RRFFQ69G5FB1"
	var err error
	next.WriteUID, err = db.TrackerStatusWriteUID(next.RootInstanceUID, next.ProjectUID, next.SourceKey, next.Locator, next.PendingEventUID, next.Target)
	require.NoError(t, err)
	require.NoError(t, db.ValidateTrackerFenceTransition(retired, next))
	unknown := prepared
	unknown.Phase = db.TrackerWriteUnknown
	require.Error(t, db.ValidateTrackerFenceTransition(unknown, retired), "unknown cannot be relabeled as unsent")
}

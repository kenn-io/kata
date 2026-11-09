package db_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
)

func TestTrackerRemoteAttemptBindsIndependentOwnerLane(t *testing.T) {
	root := trackerFenceFixture()
	root.Role = "coordinator"
	root.Remote = &db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", ArgumentsDigest: root.WriteUID, RootSequence: root.Sequence, OwnerSequence: 9, OwnerPreviousHead: root.WriteUID, RootEpoch: root.ProcessEpoch, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}
	require.NoError(t, root.Validate())
	owner, err := db.TrackerExecutorPreparation(root, "01ARZ3NDEKTSV4RRFFQ69G5FB3")
	require.NoError(t, err)
	require.Equal(t, uint64(9), owner.Sequence)
	require.Equal(t, root.WriteUID, owner.PreviousHead)
	require.NotEqual(t, db.TrackerFenceHead(root), db.TrackerFenceHead(owner))
	require.NoError(t, owner.Validate())
	changed := *root.Remote
	changed.OwnerSequence++
	next := root
	next.Remote = &changed
	next.Phase = db.TrackerWriteDispatching
	require.Error(t, db.ValidateTrackerFenceTransition(root, next), "a delayed permit cannot change its expected owner checkpoint")
	next = root
	next.Phase = db.TrackerWriteDispatching
	require.NoError(t, db.ValidateTrackerFenceTransition(root, next))
	raw, err := json.Marshal(root)
	require.NoError(t, err)
	restored, keep, err := db.TrackerMetadataForImport(db.MetaKV{Key: db.TrackerMetadataPrefix + root.ProjectUID + ".coordinator." + root.RootInstanceUID, Value: string(raw)}, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var normalized db.TrackerExecutionFence
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &normalized))
	require.Equal(t, db.TrackerWriteUnknown, normalized.Phase)
	require.Equal(t, root.Remote, normalized.Remote, "restore retains attempt identity but cannot assert non-send")
}

func TestTrackerCheckpointCutoverKeepsExistingRestoreBoundary(t *testing.T) {
	root := trackerFenceFixture()
	root.Role = "coordinator"
	root.Phase = db.TrackerWriteAcknowledged
	root.ProviderAcknowledged = true
	root.Result = []byte(`{"status":"closed"}`)
	attempt := db.TrackerRemoteAttempt{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB1", ArgumentsDigest: root.WriteUID, RootSequence: 1, OwnerSequence: 1, RootEpoch: root.ProcessEpoch, OwnerEpoch: "01ARZ3NDEKTSV4RRFFQ69G5FB2"}
	root.Remote = &attempt
	cp := db.TrackerOwnerCheckpoint{Version: 1, RootInstanceUID: root.RootInstanceUID, ProjectUID: root.ProjectUID, OwnerInstanceUID: root.Connection.OwnerInstanceUID, Selection: root.Connection, Sequence: 1, Head: db.TrackerFenceHead(root), Attempt: attempt, WriteUID: root.WriteUID, Disposition: db.TrackerWriteAcknowledged, ResultDigest: root.WriteUID}
	raw, err := json.Marshal(cp)
	require.NoError(t, err)
	record := db.MetaKV{Key: db.TrackerOwnerCheckpointKey(cp.RootInstanceUID, cp.ProjectUID, cp.OwnerInstanceUID), Value: string(raw)}
	restored, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var actual db.TrackerOwnerCheckpoint
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &actual))
	require.True(t, actual.Restored)
	current, _, err := db.TrackerMetadataForImport(record, db.ImportOptions{PreserveIssueSyncBindingEnabled: true})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(current.Value), &actual))
	require.False(t, actual.Restored, "trusted schema cutover does not invent missing history")
	current, _, err = db.TrackerMetadataForImport(restored, db.ImportOptions{PreserveIssueSyncBindingEnabled: true})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(current.Value), &actual))
	require.True(t, actual.Restored, "cutover cannot erase a preexisting restoration gap")
}

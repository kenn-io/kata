package db_test

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestTrackerAttemptRestoreIsStrictAndInactive(t *testing.T) {
	fence := trackerFenceFixture()
	fence.Phase = db.TrackerWriteNotSent
	fence.Result = []byte(`{"not_sent":true}`)
	base, err := db.TrackerExecutionFenceKey(fence.RootInstanceUID, fence.ProjectUID, fence.Role)
	require.NoError(t, err)
	raw, err := json.Marshal(fence)
	require.NoError(t, err)
	record := db.MetaKV{Key: fmt.Sprintf("%s.attempt.%s.%020d", base, fence.WriteUID, fence.Sequence), Value: string(raw)}
	restored, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var got db.TrackerExecutionFence
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &got))
	require.True(t, got.ActivationRequired)
	require.Equal(t, db.TrackerWriteNotSent, got.Phase)
	require.NoError(t, db.CheckTrackerMetadataRestoreConflict(record.Value, restored))
	for _, opts := range []db.ImportOptions{{MergeProject: true}, {NewInstance: true}} {
		_, keep, err := db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.False(t, keep)
	}
	bad := record
	bad.Key += "0"
	_, _, err = db.TrackerMetadataForImport(bad, db.ImportOptions{})
	require.Error(t, err)
	fence.Phase = db.TrackerWriteUnknown
	fence.Result = nil
	raw, err = json.Marshal(fence)
	require.NoError(t, err)
	bad = record
	bad.Value = string(raw)
	_, _, err = db.TrackerMetadataForImport(bad, db.ImportOptions{})
	require.Error(t, err)
}

func TestTrackerFenceRestoreIsInactiveAndProjectImportDropsAuthority(t *testing.T) {
	fence := trackerFenceFixture()
	fence.Phase = db.TrackerWriteDispatching
	key, err := db.TrackerExecutionFenceKey(fence.RootInstanceUID, fence.ProjectUID, fence.Role)
	require.NoError(t, err)
	raw, err := json.Marshal(fence)
	require.NoError(t, err)
	record := db.MetaKV{Key: key, Value: string(raw)}
	for _, opts := range []db.ImportOptions{{MergeProject: true}, {NewInstance: true}} {
		_, keep, err := db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.False(t, keep)
	}
	for _, opts := range []db.ImportOptions{{}, {PreserveIssueSyncBindingEnabled: true}} {
		got, keep, err := db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.True(t, keep)
		var restored db.TrackerExecutionFence
		require.NoError(t, json.Unmarshal([]byte(got.Value), &restored))
		require.True(t, restored.ActivationRequired)
		require.Equal(t, db.TrackerWriteUnknown, restored.Phase)
		require.False(t, restored.CanDispatch(restored.ProcessEpoch))
	}
}

func TestTrackerRestoreRetainsAcknowledgedHistory(t *testing.T) {
	fence := trackerFenceFixture()
	fence.Phase = db.TrackerWriteAcknowledged
	fence.ProviderAcknowledged = true
	fence.Result = []byte(`{"status":"closed"}`)
	key, err := db.TrackerExecutionFenceKey(fence.RootInstanceUID, fence.ProjectUID, fence.Role)
	require.NoError(t, err)
	raw, err := json.Marshal(fence)
	require.NoError(t, err)
	record := db.MetaKV{Key: key + ".history." + fence.WriteUID, Value: string(raw)}
	restored, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var got db.TrackerExecutionFence
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &got))
	require.True(t, got.ActivationRequired)
	require.Equal(t, db.TrackerWriteAcknowledged, got.Phase)
	require.True(t, got.ProviderAcknowledged)
	record.Key = key + ".history." + "invalid"
	_, _, err = db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.Error(t, err)
}

// Contract: replay cannot overwrite retained evidence with an older or
// different write. Canonical identical evidence can be imported idempotently.
func TestTrackerRestoreConflictFailsClosed(t *testing.T) {
	fence := trackerFenceFixture()
	key, err := db.TrackerExecutionFenceKey(fence.RootInstanceUID, fence.ProjectUID, fence.Role)
	require.NoError(t, err)
	raw, err := json.Marshal(fence)
	require.NoError(t, err)
	record, _, err := db.TrackerMetadataForImport(db.MetaKV{Key: key, Value: string(raw)}, db.ImportOptions{})
	require.NoError(t, err)
	require.NoError(t, db.CheckTrackerMetadataRestoreConflict(record.Value, record))
	other := fence
	other.Sequence = 2
	other.PreviousHead = db.TrackerFenceHead(fence)
	raw, err = json.Marshal(other)
	require.NoError(t, err)
	incoming, _, err := db.TrackerMetadataForImport(db.MetaKV{Key: key, Value: string(raw)}, db.ImportOptions{})
	require.NoError(t, err)
	require.ErrorIs(t, db.CheckTrackerMetadataRestoreConflict(record.Value, incoming), db.ErrTrackerOutcomeUnknown)
}

func TestRestoreHistoryBarrierIsLocalAndStrictlyScoped(t *testing.T) {
	f := trackerFenceFixture()
	raw, err := json.Marshal(db.TrackerRestoreBarrier{Version: 1, RootInstanceUID: f.RootInstanceUID})
	require.NoError(t, err)
	record := db.MetaKV{Key: db.TrackerMetadataPrefix + "restore_barrier." + f.RootInstanceUID, Value: string(raw)}
	_, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	for _, opts := range []db.ImportOptions{{MergeProject: true}, {NewInstance: true}} {
		_, keep, err := db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.False(t, keep)
	}
	record.Key += "." + f.ProjectUID
	_, _, err = db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.Error(t, err, "a global barrier value cannot masquerade as a project-scoped record")
}

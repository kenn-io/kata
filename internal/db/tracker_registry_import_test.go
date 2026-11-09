package db_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
)

func TestRegistryBackupCannotRestoreActiveAuthority(t *testing.T) {
	policy := tracker.RegistryPolicy{Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Provider: "notion", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	registry, err := tracker.NextRegistryRecord(nil, &policy, "01ARZ3NDEKTSV4RRFFQ69G5FB0")
	require.NoError(t, err)
	raw, err := json.Marshal(registry)
	require.NoError(t, err)
	record := db.MetaKV{Key: db.TrackerRegistryKey(policy.Selection.OwnerInstanceUID, policy.Selection.ConnectionUID), Value: string(raw)}
	restored, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var got tracker.RegistryRecord
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &got))
	require.Equal(t, tracker.RegistryRestored, got.State)
	_, err = tracker.NextRegistryRecord(&got, &policy, got.ProcessEpoch)
	require.ErrorIs(t, err, tracker.ErrGenerationRetired)
	for _, opts := range []db.ImportOptions{{MergeProject: true}, {NewInstance: true}} {
		_, keep, err = db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.False(t, keep)
	}
	require.NoError(t, db.CheckTrackerMetadataRestoreConflict(record.Value, restored))
}

// Review48128 contract: same-epoch differing registry records must not compare
// through a projection that drops policy digest or retired-generation history.
func TestRegistryRestoreConflictRetainsFullGenerationHistory(t *testing.T) {
	p := tracker.RegistryPolicy{Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Provider: "notion", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	old, err := tracker.NextRegistryRecord(nil, &p, epoch)
	require.NoError(t, err)
	key := db.TrackerRegistryKey(p.Selection.OwnerInstanceUID, p.Selection.ConnectionUID)
	oldRaw, err := json.Marshal(old)
	require.NoError(t, err)
	imported, _, err := db.TrackerMetadataForImport(db.MetaKV{Key: key, Value: string(oldRaw)}, db.ImportOptions{})
	require.NoError(t, err)
	for _, mutate := range []func(*tracker.RegistryRecord){
		func(record *tracker.RegistryRecord) {
			record.Policy.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		func(record *tracker.RegistryRecord) {
			record.Retired = []tracker.RetiredGeneration{{Generation: "old", Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}}
		},
	} {
		changed := old
		mutate(&changed)
		require.NoError(t, changed.Validate())
		raw, err := json.Marshal(changed)
		require.NoError(t, err)
		require.ErrorIs(t, db.CheckTrackerMetadataRestoreConflict(string(raw), imported), db.ErrTrackerOutcomeUnknown)
	}
}

package db_test

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"testing"
)

func TestRestoredControlCannotResumePendingSideEffects(t *testing.T) {
	c := tracker.ControlReceipt{Version: 1, RootInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAW", UID: "01ARZ3NDEKTSV4RRFFQ69G5FB0", Principal: tracker.Principal{AuthorityInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Kind: tracker.SubjectAccount, Subject: "member"}, Selection: tracker.Selection{OwnerInstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Kind: tracker.ControlBind, SourceKey: "notion:source", RequestDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: tracker.ControlPending}
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	record := db.MetaKV{Key: db.TrackerControlKey(c.RootInstanceUID, c.ProjectUID, c.UID), Value: string(raw)}
	restored, keep, err := db.TrackerMetadataForImport(record, db.ImportOptions{})
	require.NoError(t, err)
	require.True(t, keep)
	var got tracker.ControlReceipt
	require.NoError(t, json.Unmarshal([]byte(restored.Value), &got))
	require.Equal(t, tracker.ControlRestored, got.State)
	require.Equal(t, "tracker_restored_inactive", got.Result.Code)
	for _, opts := range []db.ImportOptions{{MergeProject: true}, {NewInstance: true}} {
		_, keep, err = db.TrackerMetadataForImport(record, opts)
		require.NoError(t, err)
		require.False(t, keep)
	}
	require.NoError(t, db.CheckTrackerMetadataRestoreConflict(record.Value, restored))
}

package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A push page must finish its first snapshot run and stop before the next
// supported non-snapshot event, even when the run extends beyond the limit.
func checkFederationPushSnapshotRuns(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "push-snapshots")
	if err != nil {
		return err
	}
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "work", Author: "tester"})
	if err != nil {
		return err
	}
	var uids []string
	for i, typ := range []string{
		"issue.updated", "issue.snapshot", "issue.snapshot", "project.restored", "issue.snapshot", "issue.snapshot",
		"issue.updated", "issue.snapshot", "issue.snapshot", "issue.updated", "issue.snapshot", "issue.snapshot",
	} {
		payload := jsontext.Value(fmt.Sprintf(`{"uid":%q,"title":"work","author":"tester","status":"open"}`, issue.UID))
		event := newRemoteEvent(t, project, &issue.UID, typ, "tester", store.InstanceUID(), int64(100+i), payload)
		inserted, err := store.InsertRemoteEvent(ctx, project.ID, event)
		if err != nil {
			return err
		}
		require.True(t, inserted)
		if typ != "project.restored" {
			uids = append(uids, event.EventUID)
		}
	}
	for _, limit := range []int{1, 3, 6, 100} {
		pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), created.ID, limit)
		if err != nil {
			return err
		}
		want := uids[:5]
		if limit == 1 {
			want = uids[:1]
		}
		var got []string
		for _, event := range pending {
			got = append(got, event.UID)
		}
		require.Equal(t, want, got, "limit=%d", limit)
		if limit == 3 {
			continued, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), pending[2].ID, 1)
			if err != nil {
				return err
			}
			require.Len(t, continued, 2)
			require.Equal(t, []string{uids[3], uids[4]}, []string{continued[0].UID, continued[1].UID})
			continued, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), pending[4].ID, 2)
			if err != nil {
				return err
			}
			require.Len(t, continued, 3)
			require.Equal(t, []string{uids[5], uids[6], uids[7]}, []string{continued[0].UID, continued[1].UID, continued[2].UID})
			continued, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), continued[2].ID, 2)
			if err != nil {
				return err
			}
			require.Len(t, continued, 3)
			require.Equal(t, []string{uids[8], uids[9], uids[10]}, []string{continued[0].UID, continued[1].UID, continued[2].UID})
		}
	}
	return nil
}

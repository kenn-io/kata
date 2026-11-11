package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederationAcceptedPushEvents(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "accepted-push")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, PushEnabled: true, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "work", Author: "tester"})
	require.NoError(t, err)
	accepted, err := store.FederationPushAccepted(ctx, project.ID, event.ID)
	require.NoError(t, err)
	require.False(t, accepted)
	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{event.ID}))
	reopened := backend.OpenSame(t, store)
	accepted, err = reopened.FederationPushAccepted(ctx, project.ID, event.ID)
	require.NoError(t, err)
	require.True(t, accepted)
	_, pending, err := reopened.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "pending", Author: "tester"})
	require.NoError(t, err)
	_, later, err := reopened.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later accepted", Author: "tester"})
	require.NoError(t, err)
	require.NoError(t, reopened.RecordFederationPushAccepted(ctx, project.ID, []int64{later.ID}))
	through, err := reopened.FederationPushAcceptedThrough(ctx, project.ID, reopened.InstanceUID(), 0)
	require.NoError(t, err)
	require.Equal(t, event.ID, through)
	through, err = reopened.FederationPushAcceptedThrough(ctx, project.ID, reopened.InstanceUID(), pending.ID)
	require.NoError(t, err)
	require.Equal(t, later.ID, through)
	t.Run("pending stats", func(t *testing.T) {
		count, high, err := reopened.PendingFederationPushStats(ctx, project.ID, reopened.InstanceUID(), 0)
		require.NoError(t, err)
		require.Equal(t, int64(1), count)
		require.Equal(t, pending.ID, high)
		events, err := reopened.PendingFederationPushEvents(ctx, project.ID, reopened.InstanceUID(), 0, 10)
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, pending.UID, events[0].UID)
	})
	t.Run("advance prunes covered acknowledgments", func(t *testing.T) {
		require.NoError(t, reopened.AdvanceFederationPushCursor(ctx, project.ID, event.ID))
		covered, err := reopened.FederationPushAccepted(ctx, project.ID, event.ID)
		require.NoError(t, err)
		require.False(t, covered)
		uncovered, err := reopened.FederationPushAccepted(ctx, project.ID, later.ID)
		require.NoError(t, err)
		require.True(t, uncovered)
	})
	t.Run("skip later poisoned event preserves pending work", func(t *testing.T) {
		_, poison, err := reopened.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "poison", Author: "tester"})
		require.NoError(t, err)
		q, err := reopened.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: project.ID, Direction: db.FederationQuarantineDirectionPush, FirstEventID: poison.ID, LastEventID: poison.ID, EventUIDs: []string{poison.UID}, Error: "poisoned event"})
		require.NoError(t, err)
		_, err = reopened.SkipFederationQuarantine(ctx, db.SkipFederationQuarantineParams{ID: q.ID, ProjectID: project.ID, Actor: "tester"})
		require.NoError(t, err)
		binding, err := reopened.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		require.Less(t, binding.PushCursorEventID, pending.ID)
		events, err := reopened.PendingFederationPushEvents(ctx, project.ID, reopened.InstanceUID(), binding.PushCursorEventID, 10)
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, pending.UID, events[0].UID)
	})

	return nil
}

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
	uids := make([]string, 0)
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
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, PushEnabled: true, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	metadata := newRemoteEvent(t, project, nil, "project.metadata_updated", "tester", store.InstanceUID(), 300, []byte(`{"metadata":{}}`))
	_, err = store.InsertRemoteEvent(ctx, project.ID, metadata)
	require.NoError(t, err)
	stored, err := store.EventsByUIDs(ctx, project.ID, []string{metadata.EventUID})
	require.NoError(t, err)
	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{stored[0].ID}))
	for i := range 3 {
		event := newRemoteEvent(t, project, &issue.UID, "issue.snapshot", "tester", store.InstanceUID(), int64(301+i), []byte(`{"uid":"`+issue.UID+`","title":"work","author":"tester","status":"open"}`))
		_, err = store.InsertRemoteEvent(ctx, project.ID, event)
		require.NoError(t, err)
	}
	snapshots, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), before, 2)
	require.NoError(t, err)
	require.Len(t, snapshots, 3, "accepted metadata must not truncate a snapshot run")
	return nil
}

package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederationAcceptedPushEvents(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	accepted := store
	project, err := store.CreateProject(ctx, "accepted-push")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-push")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, PushEnabled: true, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: other.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 2, HubProjectUID: other.UID, PushEnabled: true, Actor: "tester", Enabled: true})
	require.NoError(t, err)
	_, first, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	_, second, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	require.Error(t, accepted.RecordFederationPushAccepted(ctx, project.ID, []int64{first.ID, 9223372036854775807}))
	found, err := AcceptedPushRow(ctx, accepted, project.ID, first.ID)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, accepted.RecordFederationPushAccepted(ctx, project.ID, []int64{second.ID}))
	require.NoError(t, accepted.RecordFederationPushAccepted(ctx, project.ID, []int64{second.ID}))
	reopened := backend.OpenSame(t, store)
	accepted = reopened
	found, err = AcceptedPushRow(ctx, accepted, project.ID, second.ID)
	require.NoError(t, err)
	require.True(t, found)
	found, err = AcceptedPushRow(ctx, accepted, other.ID, second.ID)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, accepted.RecordFederationPushAccepted(ctx, other.ID, []int64{second.ID}))
	through, err := reopened.FederationPushAcceptedThrough(ctx, project.ID, reopened.InstanceUID(), 0)
	require.NoError(t, err)
	require.Zero(t, through)
	through, err = reopened.FederationPushAcceptedThrough(ctx, other.ID, reopened.InstanceUID(), 0)
	require.NoError(t, err)
	require.Zero(t, through)
	pending, err := reopened.PendingFederationPushEvents(ctx, project.ID, reopened.InstanceUID(), 0, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, first.ID, pending[0].ID)
	count, high, err := reopened.PendingFederationPushStats(ctx, project.ID, reopened.InstanceUID(), 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Equal(t, first.ID, high)
	require.NoError(t, reopened.AdvanceFederationPushCursor(ctx, project.ID, first.ID))
	through, err = reopened.FederationPushAcceptedThrough(ctx, project.ID, reopened.InstanceUID(), first.ID)
	require.NoError(t, err)
	require.Equal(t, second.ID, through)
	found, err = AcceptedPushRow(ctx, accepted, project.ID, second.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, reopened.AdvanceFederationPushCursor(ctx, project.ID, second.ID))
	found, err = AcceptedPushRow(ctx, accepted, project.ID, second.ID)
	require.NoError(t, err)
	require.False(t, found)
	found, err = AcceptedPushRow(ctx, accepted, other.ID, second.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, accepted.RecordFederationPushAccepted(ctx, project.ID, []int64{first.ID}))
	found, err = AcceptedPushRow(ctx, accepted, project.ID, first.ID)
	require.NoError(t, err)
	require.False(t, found)
	_, third, err := reopened.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "third", Author: "tester"})
	require.NoError(t, err)
	require.NoError(t, accepted.RecordFederationPushAccepted(ctx, project.ID, []int64{third.ID}))
	_, err = reopened.EnableFederationPush(ctx, project.ID, third.ID)
	require.NoError(t, err)
	found, err = AcceptedPushRow(ctx, accepted, project.ID, third.ID)
	require.NoError(t, err)
	require.False(t, found)
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
	barrier, err := store.EventsByUIDs(ctx, project.ID, []string{uids[5]})
	require.NoError(t, err)
	require.Len(t, barrier, 1)
	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{barrier[0].ID}))
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), created.ID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 5, "an accepted event still separates snapshot runs")
	snapshot, err := store.EventsByUIDs(ctx, project.ID, []string{uids[2]})
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	require.NoError(t, store.RecordFederationPushAccepted(ctx, project.ID, []int64{snapshot[0].ID}))
	pending, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), created.ID, 2)
	require.NoError(t, err)
	require.Len(t, pending, 4)
	require.Equal(t, []string{uids[0], uids[1], uids[3], uids[4]}, []string{pending[0].UID, pending[1].UID, pending[2].UID, pending[3].UID})
	return nil
}

// AcceptedPushRow reports whether a binding retains an event acknowledgment.
func AcceptedPushRow(ctx context.Context, store db.Storage, projectID, eventID int64) (bool, error) {
	q := store.(interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_push_accepted WHERE project_id=$1 AND event_id=$2)`, projectID, eventID).Scan(&found)
	return found, err
}

package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkRejoinRetainedHistory(t *testing.T, store db.Storage) error {
	t.Run("reuse retained snapshot", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "retained-snapshot")
		require.NoError(t, err)
		issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "task", Author: "old-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		snapshot, sourceID, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
		require.NoError(t, err)
		require.Equal(t, created.ID, sourceID)
		reused, _, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
		require.NoError(t, err)
		require.Equal(t, snapshot.UID, reused.UID)
		old, err := store.EventsByUIDs(ctx, project.ID, []string{created.UID})
		require.NoError(t, err)
		require.Equal(t, created.ContentHash, old[0].ContentHash)
	})
	t.Run("recover task moved before enrollment", func(t *testing.T) {
		ctx := t.Context()
		source, err := store.CreateProject(ctx, "offline-move-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, "offline-move-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "offline task", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "offline discussion"})
		require.NoError(t, err)
		move, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "old-actor"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: target.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		require.NoError(t, store.ResetFederatedProject(ctx, target.ID, move.Event.ID+1, move.Event.ID))
		restored, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err := store.CommentsByIssue(ctx, restored.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "offline discussion", comments[0].Body)
		snapshot, sourceID, err := store.RetainedIssueSnapshot(ctx, target.ID, issue.UID)
		require.NoError(t, err)
		require.Equal(t, move.Event.ID, sourceID)
		require.Equal(t, target.ID, snapshot.ProjectID)
	})
	t.Run("refresh boundary on upsert", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "refresh-retained-boundary")
		require.NoError(t, err)
		binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "actor-b", Enabled: true, PushEnabled: true, RetainHistory: true}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		_, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "new retained task", Author: "actor-a"})
		require.NoError(t, err)
		updated, err := store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
		_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "actor-b"})
		require.NoError(t, err)
		require.Greater(t, later.ID, event.ID)
		binding.RetainHistoryThrough = &event.ID
		updated, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
		binding.RetainHistoryThrough = nil
		binding.RetainHistory = false
		updated, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
	})
	return nil
}

func checkRejoinRetainedHistoryReset(t *testing.T, store db.Storage, backend Backend, guarded, restored bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%t-%t", guarded, restored))
	require.NoError(t, err)
	issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	other, otherEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "another task", Author: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	snapshot, sourceID, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	require.Equal(t, old.ID, sourceID)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, snapshot.ID))
	if restored {
		records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		store = backend.Open(t)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		binding.Actor = "old-actor"
		binding.RetainHistory = true
		binding, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	if guarded {
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, snapshot.ID+1, snapshot.ID, store.InstanceUID(), snapshot.ID)
	} else {
		err = store.ResetFederatedProject(ctx, project.ID, snapshot.ID+1, snapshot.ID)
	}
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
	require.Equal(t, old.OriginInstanceUID, events[0].OriginInstanceUID)
	events, err = store.EventsByUIDs(ctx, project.ID, []string{otherEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, otherEvent.ContentHash, events[0].ContentHash)
	_, err = store.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old task", got.Title)
}

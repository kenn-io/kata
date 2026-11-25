package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederationRejoinLocalEvents(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "rejoin-project")
	require.NoError(t, err)
	binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor", Enabled: true, PushEnabled: true}
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	old, retained, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	eligibleIssue, eligible, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "eligible", Author: "new-actor"})
	require.NoError(t, err)
	peer, peerEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer", Author: "new-actor"})
	require.NoError(t, err)
	parent, parentEvent, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: eligibleIssue.ID, Author: "old-actor", Body: "local parent"})
	require.NoError(t, err)
	binding.Actor = "new-actor"
	binding.PushEnabled = false
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	binding, err = store.EnableFederationPush(ctx, project.ID, cursor)
	require.NoError(t, err)
	_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local"})
	require.NoError(t, err)
	_, parentEdit, _, err := store.EditComment(ctx, db.EditCommentParams{IssueID: eligibleIssue.ID, CommentUID: parent.UID, Actor: "new-actor", Body: "edited parent"})
	require.NoError(t, err)
	require.NotNil(t, parentEdit)
	reply, replyEvent, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, nestedEvent, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: eligibleIssue.ID, Author: "new-actor", Body: "nested reply", ReplyToUID: reply.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, unrelated, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "new-actor", Body: "unrelated comment"})
	require.NoError(t, err)
	edit, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: eligibleIssue.ID, Actor: "new-actor", AddBlocks: []int64{old.ID, peer.ID}})
	require.NoError(t, err)
	require.Len(t, edit.Events, 1)
	_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
	require.NoError(t, err)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 11)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, eligible.ID)
	require.NoError(t, err)
	require.False(t, kept)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
	require.NoError(t, err)
	require.True(t, kept)
	binding, err = store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, retained.ID, binding.PushCursorEventID)
	kept, err = store.KeepFederationEventLocal(ctx, project.ID, parentEvent.ID)
	require.NoError(t, err)
	require.True(t, kept)
	pending, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Equal(t, []db.Event{eligible, peerEvent, unrelated, edit.Events[0], later}, pending)
	require.NoError(t, store.RetainFederationDependents(ctx, project.ID))
	pending, err = store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 5)
	require.Equal(t, []string{eligible.UID, peerEvent.UID, unrelated.UID, edit.Events[0].UID, later.UID}, []string{pending[0].UID, pending[1].UID, pending[2].UID, pending[3].UID, pending[4].UID})
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, later.ID))
	err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, store.InstanceUID(), later.ID)
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{retained.UID, parentEvent.UID, comment.UID, parentEdit.UID, replyEvent.UID, nestedEvent.UID})
	require.NoError(t, err)
	wantEvents := []db.Event{retained, parentEvent, comment, *parentEdit, replyEvent, nestedEvent}
	require.Len(t, events, len(wantEvents))
	for i, event := range events {
		require.Equal(t, wantEvents[i].UID, event.UID)
		require.Equal(t, wantEvents[i].ContentHash, event.ContentHash)
	}
	_, err = store.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return nil
}

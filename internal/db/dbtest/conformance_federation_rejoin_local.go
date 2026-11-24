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
	eligibleIssue, eligible, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "eligible", Author: "new-actor"})
	require.NoError(t, err)
	peer, peerEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer", Author: "new-actor"})
	require.NoError(t, err)
	old, retained, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
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
	edit, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: eligibleIssue.ID, Actor: "new-actor", AddBlocks: []int64{old.ID, peer.ID}})
	require.NoError(t, err)
	require.Len(t, edit.Events, 1)
	laterIssue, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
	require.NoError(t, err)
	validEdit, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: laterIssue.ID, Actor: "new-actor", AddRelated: []int64{peer.ID, eligibleIssue.ID}})
	require.NoError(t, err)
	require.Len(t, validEdit.Events, 1)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 4)
	require.Equal(t, []string{eligible.UID, peerEvent.UID, later.UID, validEdit.Events[0].UID}, []string{pending[0].UID, pending[1].UID, pending[2].UID, pending[3].UID})
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, validEdit.Events[0].ID))
	err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, store.InstanceUID(), validEdit.Events[0].ID)
	require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{retained.UID, comment.UID})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, []db.Event{retained, comment}, events)
	return nil
}

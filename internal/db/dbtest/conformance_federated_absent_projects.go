package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedParkedMoveRecovery(t *testing.T, _ db.Storage, backend Backend) error {
	for _, destinationFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("destination-first-%t", destinationFirst), func(t *testing.T) {
			hub := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, hub.Close()) })
			require.NoError(t, checkParkedMoveRecovery(t, hub, backend, destinationFirst))
		})
	}
	return nil
}

func checkParkedMoveRecovery(t *testing.T, hub db.Storage, backend Backend, destinationFirst bool) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "U", Author: "worker"})
	require.NoError(t, err)
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	u := issue.UID
	creations := []db.RemoteEvent{portableNativeEvent(created)}
	moves := []db.RemoteEvent{portableNativeEvent(*moved.Event)}
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, creations)
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	localU, err := spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localU.ID, Author: "worker", Body: "local comment"})
	require.NoError(t, err)
	otherIssue, later, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "local work", Author: "worker"})
	require.NoError(t, err)
	_, linkEvent, err := spoke.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: otherIssue.ID, ToIssueID: localU.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: otherIssue.ID, FromShortID: otherIssue.ShortID, FromUID: otherIssue.UID, ToShortID: localU.ShortID, ToUID: localU.UID, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, moves)
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	pending, err := spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	var ids []string
	for _, event := range pending {
		ids = append(ids, event.UID)
		require.NotEqual(t, "issue.linked", event.Type)
	}
	require.NotContains(t, ids, comment.UID)
	require.NotContains(t, ids, linkEvent.UID)
	require.Contains(t, ids, later.UID)
	sqlStore := spoke.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value='v2:deliver' WHERE key=$1`, "federation_local_only_event."+comment.UID)
	require.NoError(t, err)
	binding, err := spoke.FederationBindingByProject(ctx, route.ID)
	require.NoError(t, err)
	_, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.ErrorContains(t, err, "upgrade")
	require.ErrorContains(t, err, comment.UID)
	_, _, err = spoke.PendingFederationPushStats(ctx, route.ID, spoke.InstanceUID(), 0)
	require.ErrorContains(t, err, "upgrade")
	err = spoke.ResetFederatedProjectIfNoPendingPush(ctx, route.ID, 90, 80, spoke.InstanceUID(), 0)
	require.ErrorContains(t, err, "upgrade")
	err = spoke.ResetFederatedProject(ctx, route.ID, 90, 80)
	require.ErrorContains(t, err, "upgrade")
	after, err := spoke.FederationBindingByProject(ctx, route.ID)
	require.NoError(t, err)
	require.Equal(t, binding.PushCursorEventID, after.PushCursorEventID)
	require.Equal(t, binding.PullCursorEventID, after.PullCursorEventID)
	require.Equal(t, binding.ReplayHorizonEventID, after.ReplayHorizonEventID)
	retained, err := spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localU.ID, retained.ID)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value='v1:held' WHERE key=$1`, "federation_local_only_event."+comment.UID)
	require.NoError(t, err)
	parked, err := spoke.ProjectByUID(ctx, target.UID)
	require.NoError(t, err)
	_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, route.ID, later.ID))
	if destinationFirst {
		require.NoError(t, spoke.ResetFederatedProject(ctx, parked.ID, 0, 0))
		require.NoError(t, spoke.ResetFederatedProject(ctx, route.ID, 0, 0))
	} else {
		require.NoError(t, spoke.ResetFederatedProject(ctx, route.ID, 0, 0))
		require.NoError(t, spoke.ResetFederatedProject(ctx, parked.ID, 0, 0))
	}
	history, err := spoke.EventsByUIDs(ctx, route.ID, []string{comment.UID})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, comment.ContentHash, history[0].ContentHash)
	_, err = spoke.InsertRemoteEvents(ctx, parked.ID, append(slices.Clone(creations), moves...))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, parked.ID))
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := spoke.CommentsByIssue(ctx, localU.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "local comment", comments[0].Body)
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), later.ID, 100)
	require.NoError(t, err)
	require.NotContains(t, eventUIDs(pending), comment.UID, "joining the destination keeps the original project's edit local")
	hubIssue, err := hub.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	returned, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: hubIssue.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: hubIssue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*returned.Event)})
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), later.ID, 100)
	require.NoError(t, err)
	require.Contains(t, eventUIDs(pending), comment.UID)
	count, highWater, err := spoke.PendingFederationPushStats(ctx, route.ID, spoke.InstanceUID(), later.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Equal(t, comment.ID, highWater)
	require.ErrorIs(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, route.ID, 0, 0, spoke.InstanceUID(), later.ID), db.ErrFederationResetBlockedByPendingPush)
	otherTarget, err := hub.CreateProject(ctx, "another-target")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, otherTarget.ID, "worker")
	require.NoError(t, err)
	movedAgain, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: hubIssue.ID, FromProjectID: source.ID, ToProjectID: otherTarget.ID, IfMatchRev: returned.Issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*movedAgain.Event)})
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), later.ID, 100)
	require.NoError(t, err)
	require.NotContains(t, eventUIDs(pending), comment.UID)
	count, _, err = spoke.PendingFederationPushStats(ctx, route.ID, spoke.InstanceUID(), later.ID)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, spoke.ResetFederatedProjectIfNoPendingPush(ctx, route.ID, 0, 0, spoke.InstanceUID(), later.ID))
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value='v1:deliver' WHERE key=$1`, "federation_local_only_event."+comment.UID)
	require.NoError(t, err)
	require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, route.ID, later.ID))
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), later.ID, 100)
	require.NoError(t, err)
	require.NotContains(t, eventUIDs(pending), comment.UID, "acknowledged released events must leave the pending queue")
	return nil
}

package dbtest

import (
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
	uids, creations, moves := make([]string, 0, 3), make([]db.RemoteEvent, 0, 4), make([]db.RemoteEvent, 0, 3)
	for _, title := range []string{"U"} {
		issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: title, Author: "worker"})
		require.NoError(t, err)
		moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		uids, creations, moves = append(uids, issue.UID), append(creations, portableNativeEvent(created)), append(moves, portableNativeEvent(*moved.Event))
	}
	u := uids[0]
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
	_, err = spoke.CreateLink(ctx, db.CreateLinkParams{FromIssueID: otherIssue.ID, ToIssueID: localU.ID, Type: "blocks", Author: "worker"})
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
	require.Contains(t, ids, later.UID)
	parked, err := spoke.ProjectByUID(ctx, target.UID)
	require.NoError(t, err)
	_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
	require.NoError(t, err)
	other, err := spoke.CreateProject(ctx, "other")
	require.NoError(t, err)
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localU.ID, FromProjectID: parked.ID, ToProjectID: other.ID, IfMatchRev: localU.Revision, Actor: "worker"})
	require.NoError(t, err)
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	for _, event := range pending {
		require.NotEqual(t, comment.UID, event.UID)
	}
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localU.ID, FromProjectID: other.ID, ToProjectID: parked.ID, IfMatchRev: localU.Revision, Actor: "worker"})
	require.NoError(t, err)
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	for _, event := range pending {
		require.NotEqual(t, comment.UID, event.UID)
	}
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
	hubIssue, err := hub.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	returned, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: hubIssue.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: hubIssue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*returned.Event)})
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	for _, event := range pending {
		require.NotEqual(t, comment.UID, event.UID)
	}
	return nil
}

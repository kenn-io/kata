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
			require.NoError(t, checkParkedMoveRecovery(t, hub, backend, destinationFirst, false))
		})
	}
	t.Run("source-leave", func(t *testing.T) {
		hub := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, hub.Close()) })
		require.NoError(t, checkParkedMoveRecovery(t, hub, backend, false, true))
	})
	return nil
}

func checkParkedMoveRecovery(t *testing.T, hub db.Storage, backend Backend, destinationFirst, leaveSource bool) error {
	ctx := t.Context()
	prior, err := hub.CreateProject(ctx, "prior-project")
	require.NoError(t, err)
	source, err := hub.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	for _, project := range []db.Project{prior, source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: prior.ID, Title: "U", Author: "worker"})
	require.NoError(t, err)
	inbound, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: prior.ID, ToProjectID: source.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: inbound.Issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	u := issue.UID
	creations := []db.RemoteEvent{portableNativeEvent(created), portableNativeEvent(*inbound.Event)}
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
	missingSource, err := spoke.ProjectByUID(ctx, prior.UID)
	require.NoError(t, err)
	require.NotNil(t, missingSource.DeletedAt)
	inboundHistory, err := spoke.EventsByUIDs(ctx, missingSource.ID, []string{created.UID})
	require.NoError(t, err)
	require.Len(t, inboundHistory, 1)
	require.Equal(t, created.ContentHash, inboundHistory[0].ContentHash)
	localU, err := spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, route.ID, localU.ProjectID)
	otherIssue, later, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "local work", Author: "worker"})
	require.NoError(t, err)
	if leaveSource {
		require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, route.ID, later.ID))
	}
	_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localU.ID, Author: "worker", Body: "local comment"})
	require.NoError(t, err)
	_, linkEvent, err := spoke.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: otherIssue.ID, ToIssueID: localU.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: otherIssue.ID, FromShortID: otherIssue.ShortID, FromUID: otherIssue.UID, ToShortID: localU.ShortID, ToUID: localU.UID, Actor: "worker"})
	require.NoError(t, err)
	otherRoute, err := spoke.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: otherRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 99, HubProjectUID: otherRoute.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	otherProjectIssue, independent, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: otherRoute.ID, Title: "other local work", Author: "worker"})
	require.NoError(t, err)
	_, crossLink, err := spoke.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: otherProjectIssue.ID, ToIssueID: localU.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: otherProjectIssue.ID, FromShortID: otherProjectIssue.ShortID, FromUID: otherProjectIssue.UID, ToShortID: localU.ShortID, ToUID: localU.UID, Actor: "worker"})
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
	otherPending, err := spoke.PendingFederationPushEvents(ctx, otherRoute.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	require.NotContains(t, eventUIDs(otherPending), crossLink.UID)
	require.Contains(t, eventUIDs(otherPending), independent.UID)
	if leaveSource {
		_, err = spoke.LeaveFederationReplica(ctx, route.ID)
		require.NoError(t, err)
		_, err = spoke.FederationBindingByProject(ctx, route.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
		return nil
	}
	parked, err := spoke.ProjectByUID(ctx, target.UID)
	require.NoError(t, err)
	require.NotNil(t, parked.DeletedAt)
	_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
	require.NoError(t, err)
	policy, err := spoke.(db.ProjectAccessStorage).ProjectAccessPolicy(ctx, parked.UID)
	require.NoError(t, err)
	require.Equal(t, "teams", policy.Visibility)
	require.Empty(t, policy.TeamUIDs)
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
	_, err = spoke.AddLabel(ctx, localU.ID, "local", "worker")
	require.NoError(t, err)
	hubIssue, err := hub.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	returned, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: hubIssue.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: hubIssue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*returned.Event)})
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), later.ID, 100)
	require.NoError(t, err)
	require.NotContains(t, eventUIDs(pending), comment.UID)
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, route.ID, localU.ProjectID)
	comments, err = spoke.CommentsByIssue(ctx, localU.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "local comment", comments[0].Body)
	labels, err := spoke.LabelsByIssue(ctx, localU.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "local", labels[0].Label)
	return nil
}

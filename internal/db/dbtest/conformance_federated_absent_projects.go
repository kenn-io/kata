package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Direct pulls retain issue history and missing move endpoints atomically.
func checkFederatedAbsentProjects(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	for _, p := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, p.ID, "worker")
		require.NoError(t, err)
	}
	issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Issue", Author: "worker"})
	require.NoError(t, err)
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	creation := portableNativeEvent(created)
	move := portableNativeEvent(*moved.Event)
	t.Run("absent endpoint admission and rollback", func(t *testing.T) {
		ctx := t.Context()
		receiver := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, receiver.Close()) })
		route, err := receiver.CreateProjectWithUID(ctx, "spoke-project", target.UID)
		require.NoError(t, err)
		// Unbound routes cannot admit foreign history.
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		_, err = receiver.ProjectByUID(ctx, source.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		// A later invalid event rolls the newly retained identity back with the page.
		invalid := move
		invalid.Payload = []byte(`{}`)
		resignNativeEvent(t, &invalid)
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation, invalid})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, invalid.EventUID)
		require.ErrorContains(t, err, invalid.ProjectUID)
		_, err = receiver.ProjectByUID(ctx, source.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
		foreignProjectEvent := creation
		foreignProjectEvent.Type = "project.updated"
		foreignProjectEvent.IssueUID = nil
		resignNativeEvent(t, &foreignProjectEvent)
		// A previously processed counterpart must not bypass later event validation.
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation, foreignProjectEvent})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		_, err = receiver.ProjectByUID(ctx, source.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("identity holds", func(t *testing.T) {
		ctx := t.Context()
		receiver := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, receiver.Close()) })
		route, err := receiver.CreateProjectWithUID(ctx, "spoke-project", target.UID)
		require.NoError(t, err)
		_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		collision, err := receiver.CreateProject(ctx, "federation-"+strings.ToLower(source.UID))
		require.NoError(t, err)
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
		require.NoError(t, err)
		retained, err := receiver.ProjectByUID(ctx, source.UID)
		require.NoError(t, err)
		require.NotNil(t, retained.DeletedAt)
		require.NotEqual(t, collision.Name, retained.Name)
		_, err = receiver.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: retained.ID, Actor: "worker"})
		require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
		_, err = receiver.FederationBindingByProject(ctx, retained.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		require.NoError(t, receiver.MaterializeFederatedProject(ctx, route.ID))
		got, err := receiver.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, route.ID, got.ProjectID)
		other := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, other.Close()) })
		otherRoute, err := other.CreateProjectWithUID(ctx, "spoke-project", target.UID)
		require.NoError(t, err)
		_, err = other.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: otherRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = other.InsertRemoteEvents(ctx, otherRoute.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		endpoint, err := other.ProjectByUID(ctx, source.UID)
		require.NoError(t, err)
		require.NotNil(t, endpoint.DeletedAt)
		_, err = other.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: endpoint.ID, Actor: "worker"})
		require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)

		// A cold source route retains the outbound move even without an issue row.
		cold := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, cold.Close()) })
		coldRoute, err := cold.CreateProjectWithUID(ctx, "cold-source", source.UID)
		require.NoError(t, err)
		existing, err := cold.CreateProjectWithUID(ctx, "existing-target", target.UID)
		require.NoError(t, err)
		_, _, err = cold.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: existing.ID, Actor: "worker"})
		require.NoError(t, err)
		_, err = cold.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: coldRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = cold.InsertRemoteEvents(ctx, coldRoute.ID, []db.RemoteEvent{creation, move})
		require.NoError(t, err)
		_, err = cold.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: existing.ID, Actor: "worker"})
		require.ErrorIs(t, err, db.ErrFederationProjectIdentityRequired)
		require.NoError(t, cold.MaterializeFederatedProject(ctx, coldRoute.ID))
		_, err = cold.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.ErrorIs(t, err, db.ErrNotFound)

		// A joined counterpart does not create an identity hold on its project.
		purged := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, purged.Close()) })
		purgeSource, err := purged.CreateProjectWithUID(ctx, "purge-source", source.UID)
		require.NoError(t, err)
		purgeTarget, err := purged.CreateProjectWithUID(ctx, "purge-target", target.UID)
		require.NoError(t, err)
		for _, p := range []db.Project{purgeSource, purgeTarget} {
			_, err = purged.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: p.ID, HubProjectUID: p.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
		}
		_, err = purged.InsertRemoteEvents(ctx, purgeSource.ID, []db.RemoteEvent{creation})
		require.NoError(t, err)
		require.NoError(t, purged.MaterializeFederatedProject(ctx, purgeSource.ID))
		_, err = purged.InsertRemoteEvents(ctx, purgeSource.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		require.NoError(t, purged.MaterializeFederatedProject(ctx, purgeSource.ID))
		_, err = purged.LeaveFederationReplica(ctx, purgeTarget.ID)
		require.NoError(t, err)
		purgeIssue, err := purged.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = purged.PurgeIssue(ctx, purgeIssue.ID, "worker", nil)
		require.NoError(t, err)
		_, _, err = purged.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: purgeTarget.ID, Actor: "worker"})
		require.NoError(t, err)
		_, err = purged.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: purgeTarget.ID, Actor: "worker"})
		require.NoError(t, err)
	})
	t.Run("outbound move retention", func(t *testing.T) {
		ctx := t.Context()
		// A standalone peer link and local edit survive an outbound move.
		edited := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, edited.Close()) })
		editedRoute, err := edited.CreateProjectWithUID(ctx, "edited-source", source.UID)
		require.NoError(t, err)
		_, err = edited.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: editedRoute.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{creation})
		require.NoError(t, err)
		require.NoError(t, edited.MaterializeFederatedProject(ctx, editedRoute.ID))
		local, err := edited.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		title := "Local edit"
		_, edit, _, err := edited.EditIssue(ctx, db.EditIssueParams{IssueID: local.ID, Title: &title, Actor: "worker"})
		require.NoError(t, err)
		require.NotNil(t, edit.IssueID)
		peerProject, err := edited.CreateProject(ctx, "standalone-project")
		require.NoError(t, err)
		peer, _, err := edited.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "Peer", Author: "worker"})
		require.NoError(t, err)
		_, _, err = edited.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: local.ID, ToIssueID: peer.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: peer.ID, FromShortID: peer.ShortID, FromUID: peer.UID, ToShortID: local.ShortID, ToUID: local.UID, Actor: "worker"})
		require.NoError(t, err)
		_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		require.NoError(t, edited.MaterializeFederatedProject(ctx, editedRoute.ID))
		links, err := edited.LinksByIssue(ctx, peer.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
		history, err := edited.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
		require.NoError(t, err)
		for eventUID, hash := range map[string]string{creation.EventUID: creation.ContentHash, move.EventUID: move.ContentHash, edit.UID: edit.ContentHash} {
			var found bool
			for _, event := range history {
				found = found || event.UID == eventUID && event.ContentHash == hash
			}
			require.True(t, found, "history must retain event %s", eventUID)
		}

	})

	return nil
}

package dbtest

import (
	"strings"
	"testing"
	"time"

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
		invalid = creation
		invalid.IssueUID = nil
		resignNativeEvent(t, &invalid)
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{invalid})
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
		require.ErrorContains(t, err, invalid.EventUID)
		require.ErrorContains(t, err, invalid.ProjectUID)
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

	})
	t.Run("restored unjoined destination", func(t *testing.T) {
		issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Before", Author: "worker"})
		require.NoError(t, err)
		creation := portableNativeEvent(created)
		title := "Edited on hub"
		issue, edit, _, err := hub.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
		require.NoError(t, err)
		_, hubComment, err := hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Hub comment"})
		require.NoError(t, err)
		_, hubLabel, err := hub.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "hub-label", Actor: "worker"})
		require.NoError(t, err)
		_, _, err = hub.AddLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "removed", Actor: "worker"})
		require.NoError(t, err)
		removed, err := hub.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "removed", Actor: "worker"})
		require.NoError(t, err)
		moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		move := portableNativeEvent(*moved.Event)
		seed, seedCreated, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Seed", Author: "worker"})
		require.NoError(t, err)
		seedMove, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: seed.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: seed.Revision, Actor: "worker"})
		require.NoError(t, err)
		for _, edited := range []bool{false, true} {
			t.Run(map[bool]string{false: "archived destination", true: "restored destination"}[edited], func(t *testing.T) {
				ctx := t.Context()
				spoke := backend.Open(t)
				t.Cleanup(func() { require.NoError(t, spoke.Close()) })
				route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
				require.NoError(t, err)
				_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
				require.NoError(t, err)
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
				require.NoError(t, err)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				localID := local.ID
				_, err = spoke.AddLabel(ctx, local.ID, "removed", "worker")
				require.NoError(t, err)
				_, err = spoke.AddLabel(ctx, local.ID, "local", "worker")
				require.NoError(t, err)
				if edited {
					_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Local comment"})
					require.NoError(t, err)
				}
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(seedCreated), portableNativeEvent(*seedMove.Event)})
				require.NoError(t, err)
				placeholder, err := spoke.ProjectByUID(ctx, target.UID)
				require.NoError(t, err)
				if edited {
					_, _, _, err = spoke.RestoreProject(ctx, placeholder.ID, "worker")
					require.NoError(t, err)
					_, err = spoke.LeaveFederationReplica(ctx, placeholder.ID)
					require.NoError(t, err)
				}
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*edit), portableNativeEvent(hubComment), portableNativeEvent(hubLabel), portableNativeEvent(removed), move})
				require.NoError(t, err)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				local, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, placeholder.ID, local.ProjectID)
				require.Equal(t, localID, local.ID)
				require.Equal(t, "Edited on hub", local.Title)
				labels, err := spoke.LabelsByIssue(ctx, local.ID)
				require.NoError(t, err)
				require.Len(t, labels, 2)
				require.ElementsMatch(t, []string{"hub-label", "local"}, []string{labels[0].Label, labels[1].Label})
				listed, err := spoke.ListIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
				require.NoError(t, err)
				require.Empty(t, listed)
				count, err := spoke.CountIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
				require.NoError(t, err)
				require.Zero(t, count)
				comments, err := spoke.CommentsByIssue(ctx, local.ID)
				require.NoError(t, err)
				bodies := []string{"Hub comment"}
				if edited {
					bodies = append(bodies, "Local comment")
				}
				var gotBodies []string
				for _, comment := range comments {
					gotBodies = append(gotBodies, comment.Body)
				}
				require.ElementsMatch(t, bodies, gotBodies)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				again, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, local, again)
				againComments, err := spoke.CommentsByIssue(ctx, local.ID)
				require.NoError(t, err)
				require.Equal(t, comments, againComments)
				history, err := spoke.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
				require.NoError(t, err)
				for _, want := range []db.RemoteEvent{creation, portableNativeEvent(*edit), portableNativeEvent(hubComment), portableNativeEvent(hubLabel), portableNativeEvent(removed), move} {
					var found bool
					for _, event := range history {
						if event.UID == want.EventUID {
							require.Equal(t, want.ContentHash, event.ContentHash)
							found = true
						}
					}
					require.True(t, found)
				}
			})
		}
		t.Run("joined then detached destination", func(t *testing.T) {
			ctx := t.Context()
			spoke := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, spoke.Close()) })
			route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation, portableNativeEvent(seedCreated), portableNativeEvent(*seedMove.Event)})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
			local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Local comment"})
			require.NoError(t, err)
			placeholder, err := spoke.ProjectByUID(ctx, target.UID)
			require.NoError(t, err)
			_, _, _, err = spoke.RestoreProject(ctx, placeholder.ID, "worker")
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: placeholder.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
			_, err = spoke.LeaveFederationReplica(ctx, placeholder.ID)
			require.NoError(t, err)
			unrelated, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: placeholder.ID, Title: "Standalone issue", Author: "worker"})
			require.NoError(t, err)
			_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: unrelated.ID, Author: "worker", Body: "Standalone comment"})
			require.NoError(t, err)
			_, err = spoke.AddLabel(ctx, unrelated.ID, "standalone", "worker")
			require.NoError(t, err)
			unrelated, err = spoke.IssueByUID(ctx, unrelated.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*edit), portableNativeEvent(hubComment), portableNativeEvent(hubLabel), move})
			require.NoError(t, err)
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
			local, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, placeholder.ID, local.ProjectID)
			got, err := spoke.IssueByUID(ctx, unrelated.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, unrelated, got)
			comments, err := spoke.CommentsByIssue(ctx, unrelated.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "Standalone comment", comments[0].Body)
			labels, err := spoke.LabelsByIssue(ctx, unrelated.ID)
			require.NoError(t, err)
			require.Len(t, labels, 1)
			require.Equal(t, "standalone", labels[0].Label)
		})
	})
	t.Run("outbound move retention", func(t *testing.T) {
		ctx := t.Context()
		// Local edits survive an outbound move.
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

		now := time.Now().UTC()
		root, _, err := edited.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: editedRoute.ID, IssueID: local.ID, ConnectorInstance: "notes",
			ExternalRootKey: "root", ExternalAccountKey: "account", Actor: "worker", ReceiveCommentsAfter: now,
		})
		require.NoError(t, err)
		root, claimed, err := edited.ClaimExternalRootBinding(ctx, root.ID, "worker-token", now, now.Add(-db.ExternalRootClaimStaleAfter))
		require.NoError(t, err)
		require.True(t, claimed)
		comment, _, changed, err := edited.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
			BindingID: root.ID, ClaimToken: root.ClaimToken, ExternalID: "comment", ExternalRevision: "one",
			Body: "External comment", ExternalActorID: "person", ExternalActorName: "Person",
			ExternalCreatedAt: now, ExternalUpdatedAt: now, IntegrationActor: "connector:notes",
		})
		require.NoError(t, err)
		require.True(t, changed)
		ordinary, _, err := edited.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Local comment"})
		require.NoError(t, err)
		_, err = edited.ReleaseExternalRootClaim(ctx, root.ID, root.ClaimToken)
		require.NoError(t, err)
		_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		seen, err := edited.MaxEventID(ctx)
		require.NoError(t, err)
		applyCtx, capture := db.WithFederationMoveResetCapture(ctx)
		applyCtx = db.WithFederationPulledEventUIDs(applyCtx, []string{move.EventUID})
		require.NoError(t, edited.MaterializeFederatedProject(applyCtx, editedRoute.ID))
		reset, err := edited.PurgeResetCheck(ctx, seen, editedRoute.ID)
		require.NoError(t, err)
		require.Greater(t, reset, seen)
		require.Contains(t, capture.Resets, db.FederationProjectReset{ProjectID: editedRoute.ID, EventID: reset})

		comments, err := edited.CommentsByIssue(ctx, local.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		require.ElementsMatch(t, []string{comment.UID, ordinary.UID}, []string{comments[0].UID, comments[1].UID})
		parked, err := edited.ProjectByUID(ctx, target.UID)
		require.NoError(t, err)
		require.NotNil(t, parked.DeletedAt)
		local, err = edited.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, parked.ID, local.ProjectID)

		history, err := edited.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
		require.NoError(t, err)
		for eventUID, hash := range map[string]string{creation.EventUID: creation.ContentHash, move.EventUID: move.ContentHash, edit.UID: edit.ContentHash} {
			var found bool
			for _, event := range history {
				found = found || event.UID == eventUID && event.ContentHash == hash
			}
			require.True(t, found, "history must retain event %s", eventUID)
		}
		_, _, _, err = edited.RestoreProject(ctx, parked.ID, "worker")
		require.NoError(t, err)
		_, err = edited.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		current, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		awayOwner := "away-owner"
		current, awayEdit, _, err := hub.EditIssue(ctx, db.EditIssueParams{IssueID: current.ID, Owner: &awayOwner, Actor: "worker"})
		require.NoError(t, err)
		returned, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: current.Revision, Actor: "worker"})
		require.NoError(t, err)
		_, err = edited.InsertRemoteEvents(ctx, editedRoute.ID, []db.RemoteEvent{portableNativeEvent(*awayEdit), portableNativeEvent(*returned.Event)})
		require.NoError(t, err)
		require.NoError(t, edited.MaterializeFederatedProject(ctx, editedRoute.ID))
		restored, err := edited.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, editedRoute.ID, restored.ProjectID)
		comments, err = edited.CommentsByIssue(ctx, restored.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		require.NotNil(t, restored.Owner)
		require.Equal(t, awayOwner, *restored.Owner)
	})

	return nil
}

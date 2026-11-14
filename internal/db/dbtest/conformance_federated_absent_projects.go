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
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{move})
		require.NoError(t, err)
		require.NoError(t, receiver.MaterializeFederatedProject(ctx, route.ID))
		got, err := receiver.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, route.ID, got.ProjectID)

		// A pre-existing archived endpoint keeps its identity even without an issue row.
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
	t.Run("unjoined destination keeps hub edits", func(t *testing.T) {
		ctx := t.Context()
		issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Before", Author: "worker"})
		require.NoError(t, err)
		peer, peerCreated, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Peer", Author: "worker"})
		require.NoError(t, err)
		linkParams := db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "worker"}
		link, linked, err := hub.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "worker"}, linkParams)
		require.NoError(t, err)
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
		linkParams.EventType = "issue.unlinked"
		unlinked, err := hub.DeleteLinkAndEvent(ctx, link, linkParams)
		require.NoError(t, err)
		moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		move := portableNativeEvent(*moved.Event)
		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(created), portableNativeEvent(peerCreated), portableNativeEvent(linked)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		for _, label := range []string{"removed", "local"} {
			_, err = spoke.AddLabel(ctx, local.ID, label, "worker")
			require.NoError(t, err)
		}
		_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Local comment"})
		require.NoError(t, err)
		pulled := []db.Event{*edit, hubComment, hubLabel, removed, unlinked, *moved.Event}
		var remote []db.RemoteEvent
		for _, event := range pulled {
			remote = append(remote, portableNativeEvent(event))
		}
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, remote)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		retained, err := spoke.ProjectByUID(ctx, target.UID)
		require.NoError(t, err)
		require.NotNil(t, retained.DeletedAt)
		check := func(want db.Issue) {
			got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, want.ID, got.ID)
			require.Equal(t, want.ProjectID, got.ProjectID)
			require.Equal(t, "Edited on hub", got.Title)
			labels, err := spoke.LabelsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, labels, 2)
			require.ElementsMatch(t, []string{"hub-label", "local"}, []string{labels[0].Label, labels[1].Label})
			comments, err := spoke.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 2)
			require.ElementsMatch(t, []string{"Hub comment", "Local comment"}, []string{comments[0].Body, comments[1].Body})
			links, err := spoke.LinksByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Empty(t, links)
		}
		check(db.Issue{ID: local.ID, ProjectID: retained.ID})
		listed, err := spoke.ListIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
		require.NoError(t, err)
		require.Len(t, listed, 1, "only the peer remains in the source project")
		count, err := spoke.CountIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)

		// Restoring the counterpart keeps its issue and leaves standalone work alone.
		_, _, _, err = spoke.RestoreProject(ctx, retained.ID, "worker")
		require.NoError(t, err)
		standalone, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: retained.ID, Title: "Standalone issue", Author: "worker"})
		require.NoError(t, err)
		_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: standalone.ID, Author: "worker", Body: "Standalone comment"})
		require.NoError(t, err)
		_, err = spoke.AddLabel(ctx, standalone.ID, "standalone", "worker")
		require.NoError(t, err)
		standalone, err = spoke.IssueByUID(ctx, standalone.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		check(db.Issue{ID: local.ID, ProjectID: retained.ID})
		got, err := spoke.IssueByUID(ctx, standalone.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, standalone, got)
		comments, err := spoke.CommentsByIssue(ctx, standalone.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		labels, err := spoke.LabelsByIssue(ctx, standalone.ID)
		require.NoError(t, err)
		require.Len(t, labels, 1)

		// A later winning move from the same source takes the retained issue along.
		other, err := hub.CreateProject(ctx, "other-project")
		require.NoError(t, err)
		winner := newRemoteEvent(t, other, &issue.UID, "issue.moved", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1000, []byte(strings.ReplaceAll(string(move.Payload), target.UID, other.UID)))
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{winner})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		current, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		winnerProject, err := spoke.ProjectByUID(ctx, other.UID)
		require.NoError(t, err)
		require.Equal(t, winnerProject.ID, current.ProjectID)
		history, err := spoke.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
		require.NoError(t, err)
		for _, want := range append([]db.RemoteEvent{portableNativeEvent(created)}, remote...) {
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

	return nil
}

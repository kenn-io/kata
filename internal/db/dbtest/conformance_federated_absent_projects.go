package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func labelsValues(labels []db.IssueLabel) []string {
	values := make([]string, 0, len(labels))
	for _, label := range labels {
		values = append(values, label.Label)
	}
	return values
}

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
		badHash := creation
		badHash.ContentHash = "invalid"
		_, err = receiver.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{badHash})
		require.ErrorContains(t, err, "validate remote event")
		require.ErrorContains(t, err, creation.ProjectUID)
		require.ErrorContains(t, err, creation.EventUID)
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
		query := receiver.(interface {
			QueryRowContext(context.Context, string, ...any) *sql.Row
		})
		var marker bool
		require.NoError(t, query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta WHERE key=$1)`, db.FederationRetainedProjectPrefix+source.UID).Scan(&marker))
		require.True(t, marker)
		_, err = receiver.LeaveFederationReplica(ctx, route.ID)
		require.NoError(t, err)
		_, err = receiver.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: retained.ID, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta WHERE key=$1)`, db.FederationRetainedProjectPrefix+source.UID).Scan(&marker))
		require.False(t, marker)

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

	})
	t.Run("unjoined destination keeps hub edits", func(t *testing.T) {
		ctx := t.Context()
		issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Before", Author: "worker", Metadata: map[string]jsontext.Value{"test": jsontext.Value(`"value"`)}})
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
		moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(created)})
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
		pulled := []db.Event{peerCreated, *edit, hubComment, hubLabel, removed, linked, *moved.Event}
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
		got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, local.ID, got.ID)
		require.Equal(t, retained.ID, got.ProjectID)
		require.Equal(t, "Edited on hub", got.Title)
		require.JSONEq(t, `{"test":"value"}`, string(got.Metadata))
		labels, err := spoke.LabelsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"hub-label", "local"}, labelsValues(labels))
		comments, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"Hub comment", "Local comment"}, importCommentBodies(comments))
		links, err := spoke.LinksByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
		_, delayed, err := hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Delayed comment"})
		require.NoError(t, err)
		linkParams.EventType = "issue.unlinked"
		unlinked, err := hub.DeleteLinkAndEvent(ctx, link, linkParams)
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(delayed), portableNativeEvent(unlinked)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		comments, err = spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"Hub comment", "Local comment", "Delayed comment"}, importCommentBodies(comments))
		links, err = spoke.LinksByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Empty(t, links)
		listed, err := spoke.ListIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
		require.NoError(t, err)
		require.Len(t, listed, 1, "only the peer remains in the source project")
		count, err := spoke.CountIssues(ctx, db.ListIssuesParams{ProjectID: route.ID})
		require.NoError(t, err)
		require.EqualValues(t, 1, count)

		_, _, _, err = spoke.RestoreProject(ctx, retained.ID, "worker")
		require.NoError(t, err)
		unrelated, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: retained.ID, Title: "Unrelated", Author: "worker"})
		require.NoError(t, err)
		_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: unrelated.ID, Author: "worker", Body: "Unrelated comment"})
		require.NoError(t, err)
		_, _, err = spoke.AddLabelAndEvent(ctx, unrelated.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "unrelated", Actor: "worker"})
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: retained.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, retained.ID))
		got, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, local.ID, got.ID)
		require.Equal(t, retained.ID, got.ProjectID)
		comments, err = spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"Hub comment", "Local comment", "Delayed comment"}, importCommentBodies(comments))
		labels, err = spoke.LabelsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"hub-label", "local"}, labelsValues(labels))
		_, err = spoke.IssueByUID(ctx, unrelated.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		otherComments, err := spoke.CommentsByIssue(ctx, unrelated.ID)
		require.NoError(t, err)
		require.Len(t, otherComments, 1)
		require.Equal(t, "Unrelated comment", otherComments[0].Body)
		otherLabels, err := spoke.LabelsByIssue(ctx, unrelated.ID)
		require.NoError(t, err)
		require.Len(t, otherLabels, 1)
		require.Equal(t, "unrelated", otherLabels[0].Label)
		links, err = spoke.LinksByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Empty(t, links)
		history, err := spoke.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
		require.NoError(t, err)
		for _, want := range append([]db.RemoteEvent{portableNativeEvent(created)}, remote...) {
			if want.IssueUID == nil || *want.IssueUID != issue.UID {
				continue
			}
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

	t.Run("later move after parking", func(t *testing.T) {
		ctx := t.Context()
		other, err := hub.CreateProject(ctx, "later-destination")
		require.NoError(t, err)
		_, err = hub.EnableProjectFederation(ctx, other.ID, "worker")
		require.NoError(t, err)
		current, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		winner, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: target.ID, ToProjectID: other.ID, IfMatchRev: current.Revision, Actor: "worker"})
		require.NoError(t, err)
		for _, restore := range []string{"none", "current", "older"} {
			t.Run(restore, func(t *testing.T) {
				spoke := backend.Open(t)
				t.Cleanup(func() { require.NoError(t, spoke.Close()) })
				route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
				require.NoError(t, err)
				_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, Actor: "worker"})
				require.NoError(t, err)
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation})
				require.NoError(t, err)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{move})
				require.NoError(t, err)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				retained, err := spoke.ProjectByUID(ctx, target.UID)
				require.NoError(t, err)
				if restore != "none" {
					_, _, _, err = spoke.RestoreProject(ctx, retained.ID, "worker")
					require.NoError(t, err)
					if restore == "older" {
						// Older restores left the retained marker behind.
						writer := spoke.(interface {
							ExecContext(context.Context, string, ...any) (sql.Result, error)
						})
						_, err = writer.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,'1') ON CONFLICT(key) DO NOTHING`, db.FederationRetainedProjectPrefix+target.UID)
						require.NoError(t, err)
					}
					if restore == "current" {
						_, _, err = spoke.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: retained.ID, Actor: "worker", Force: true})
						require.NoError(t, err)
					}
				}
				_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{portableNativeEvent(*winner.Event)})
				require.NoError(t, err)
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
				got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				if restore != "none" {
					require.Equal(t, retained.ID, got.ProjectID)
				} else {
					winnerProject, err := spoke.ProjectByUID(ctx, other.UID)
					require.NoError(t, err)
					require.Equal(t, winnerProject.ID, got.ProjectID)
				}
			})
		}
	})

	t.Run("parked issue rejects cycle with local parent", func(t *testing.T) {
		ctx := t.Context()
		peer, peerCreated, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Peer for cycle", Author: "worker"})
		require.NoError(t, err)
		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, Actor: "worker"})
		require.NoError(t, err)
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{creation, portableNativeEvent(peerCreated)})
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
		local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localPeer, err := spoke.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = spoke.CreateLink(ctx, db.CreateLinkParams{FromIssueID: localPeer.ID, ToIssueID: local.ID, Type: "parent", Author: "worker"})
		require.NoError(t, err)
		first := newRemoteEvent(t, source, &issue.UID, "issue.linked", "worker", move.OriginInstanceUID, move.HLCPhysicalMS-1, []byte(fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"parent"}`, issue.UID, peer.UID)))
		_, err = spoke.InsertRemoteEvents(ctx, route.ID, []db.RemoteEvent{first, move})
		require.NoError(t, err)
		err = spoke.MaterializeFederatedProject(ctx, route.ID)
		require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	})

	return nil
}

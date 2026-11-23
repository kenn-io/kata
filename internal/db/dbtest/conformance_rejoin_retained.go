package dbtest

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkRejoinRetainedHistory(t *testing.T, store db.Storage) error {
	t.Run("recover comment from retained snapshot", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "retained-comment-snapshot")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "task", Author: "old-actor"})
		require.NoError(t, err)
		peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer", Author: "old-actor"})
		require.NoError(t, err)
		parent, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "old-actor", Body: "parent"})
		require.NoError(t, err)
		reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
		require.NoError(t, err)
		_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor"})
		require.NoError(t, err)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		copy, _, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		var payload struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(copy.Payload), &payload))
		require.Equal(t, parent.UID, payload.ReplyToUID)
		require.Equal(t, "reply", payload.ReplyKind)
		require.NotNil(t, copy.RelatedIssueUID)
		require.Equal(t, peer.UID, *copy.RelatedIssueUID)
	})
	t.Run("recover retained comment", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "recover-retained-comment")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "task", Author: "old-actor"})
		require.NoError(t, err)
		parent, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "parent"})
		require.NoError(t, err)
		reply, original, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		_, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: reply.UID, Actor: "new-actor", Body: "edited reply"})
		require.NoError(t, err)
		copy, sourceID, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		require.Equal(t, original.ID, sourceID)
		require.Equal(t, "new-actor", copy.Actor)
		var payload struct {
			Author     string `json:"author"`
			Body       string `json:"body"`
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(copy.Payload), &payload))
		require.Equal(t, "new-actor", payload.Author)
		require.Equal(t, "edited reply", payload.Body)
		require.Equal(t, parent.UID, payload.ReplyToUID)
		require.Equal(t, "reply", payload.ReplyKind)
		again, _, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		require.Equal(t, copy.UID, again.UID)
		retained, err := store.EventsByUIDs(ctx, project.ID, []string{original.UID})
		require.NoError(t, err)
		require.Len(t, retained, 1)
		require.Equal(t, original.ContentHash, retained[0].ContentHash)
		_, _, err = store.RetainedCommentEvent(ctx, project.ID, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA")
		require.ErrorIs(t, err, db.ErrNotFound)
	})
	t.Run("count retained events", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "count-retained-events")
		require.NoError(t, err)
		_, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old", Author: "old-actor"})
		require.NoError(t, err)
		_, same, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "same", Author: "new-actor"})
		require.NoError(t, err)
		_, later, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "old-actor"})
		require.NoError(t, err)
		binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		binding.PushCursorEventID = same.ID
		count, err := store.CountRetainedFederationEvents(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, int64(1), count)
		binding.PushCursorEventID = later.ID
		binding.RetainedThroughEventID = old.ID
		count, err = store.CountRetainedFederationEvents(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, int64(1), count)
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
		binding.RetainHistory = false
		updated, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.Equal(t, event.ID, updated.RetainedThroughEventID)
	})
	t.Run("return to earlier actor", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "return-to-actor")
		require.NoError(t, err)
		issue, original, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained task", Author: "actor-a"})
		require.NoError(t, err)
		binding := db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "actor-b", Enabled: true, PushEnabled: true, RetainHistory: true}
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, original.ID))
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		binding.Actor = "actor-a"
		_, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.NoError(t, store.ResetFederatedProject(ctx, project.ID, 1, 0))
		events, err := store.EventsByUIDs(ctx, project.ID, []string{original.UID})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, original.ContentHash, events[0].ContentHash)
		_, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
	})
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint("guarded=", guarded), func(t *testing.T) {
			checkRejoinRetainedHistoryReset(t, store, guarded)
		})
	}
	t.Run("incoming link", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "incoming-link-project")
		require.NoError(t, err)
		first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first", Author: "old-actor"})
		require.NoError(t, err)
		second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: second.ID, ToIssueID: first.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: second.ID, Actor: "old-actor", FromUID: second.UID, ToUID: first.UID, FromShortID: second.ShortID, ToShortID: first.ShortID})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
		require.NoError(t, err)
		snapshot, _, err := store.RetainedIssueSnapshot(ctx, project.ID, first.UID)
		require.NoError(t, err)
		var payload db.IssueDocumentPayload
		require.NoError(t, json.Unmarshal([]byte(snapshot.Payload), &payload))
		require.Len(t, payload.Links, 1)
		require.True(t, payload.Links[0].Incoming)
		require.Equal(t, second.UID, payload.Links[0].ToIssueUID)
	})
	for _, state := range []string{"absent", "present", "retired"} {
		t.Run("move baseline "+state, func(t *testing.T) {
			ctx := t.Context()
			source, err := store.CreateProject(ctx, "move-baseline-source-"+state)
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "move-baseline-target-"+state)
			require.NoError(t, err)
			issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: source.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
			require.NoError(t, err)
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 2, HubProjectUID: target.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			if state != "absent" {
				snapshot, _, err := store.RetainedIssueSnapshot(ctx, source.ID, issue.UID)
				require.NoError(t, err)
				if state == "retired" {
					require.NoError(t, store.AcknowledgeFederationPush(ctx, source.ID, snapshot.ID, []string{snapshot.UID}))
				}
			}
			moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "new-actor"})
			require.NoError(t, err)
			var payload struct {
				Snapshot db.IssueDocumentPayload `json:"snapshot"`
			}
			require.NoError(t, json.Unmarshal([]byte(moved.Event.Payload), &payload))
			want := "old-actor"
			if state == "present" {
				want = "new-actor"
			}
			require.Equal(t, want, payload.Snapshot.Author)
			events, err := store.EventsByUIDs(ctx, source.ID, []string{old.UID})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, old.ContentHash, events[0].ContentHash)
		})
	}
	return nil
}

func checkRejoinRetainedHistoryReset(t *testing.T, store db.Storage, guarded bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%t", guarded))
	require.NoError(t, err)
	issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.RetainedThroughEventID, old.ID)
	snapshot, sourceID, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	require.Equal(t, old.ID, sourceID)
	again, _, err := store.RetainedIssueSnapshot(ctx, project.ID, issue.UID)
	require.NoError(t, err)
	require.Equal(t, snapshot.UID, again.UID)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, snapshot.ID))
	if guarded {
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, snapshot.ID+1, snapshot.ID, store.InstanceUID(), snapshot.ID)
	} else {
		err = store.ResetFederatedProject(ctx, project.ID, snapshot.ID+1, snapshot.ID)
	}
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old task", got.Title)
}

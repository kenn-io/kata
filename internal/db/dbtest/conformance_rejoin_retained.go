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
		recovered, _, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		var payload struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(recovered.Payload), &payload))
		require.Equal(t, parent.UID, payload.ReplyToUID)
		require.Equal(t, "reply", payload.ReplyKind)
		require.NotNil(t, recovered.RelatedIssueUID)
		require.Equal(t, peer.UID, *recovered.RelatedIssueUID)
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
		var published []db.Event
		ctx = db.WithRecoveryEventPublisher(ctx, func(event db.Event) { published = append(published, event) })
		recovered, sourceID, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		require.Equal(t, []db.Event{recovered}, published)
		require.Equal(t, original.ID, sourceID)
		require.Equal(t, "new-actor", recovered.Actor)
		var payload struct {
			Author     string `json:"author"`
			Body       string `json:"body"`
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		require.NoError(t, json.Unmarshal([]byte(recovered.Payload), &payload))
		require.Equal(t, "new-actor", payload.Author)
		require.Equal(t, "edited reply", payload.Body)
		require.Equal(t, parent.UID, payload.ReplyToUID)
		require.Equal(t, "reply", payload.ReplyKind)
		again, _, err := store.RetainedCommentEvent(ctx, project.ID, issue.UID, reply.UID)
		require.NoError(t, err)
		require.Equal(t, recovered.UID, again.UID)
		require.Equal(t, []db.Event{recovered}, published)
		retained, err := store.EventsByUIDs(ctx, project.ID, []string{original.UID})
		require.NoError(t, err)
		require.Len(t, retained, 1)
		require.Equal(t, original.ContentHash, retained[0].ContentHash)
		_, _, err = store.RetainedCommentEvent(ctx, project.ID, issue.UID, "01AAAAAAAAAAAAAAAAAAAAAAAA")
		require.ErrorIs(t, err, db.ErrNotFound)
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
		var published []db.Event
		ctx = db.WithRecoveryEventPublisher(ctx, func(event db.Event) { published = append(published, event) })
		snapshot, _, err := store.RetainedIssueSnapshot(ctx, project.ID, first.UID)
		require.NoError(t, err)
		require.Equal(t, []db.Event{snapshot}, published)
		_, _, err = store.RetainedIssueSnapshot(ctx, project.ID, first.UID)
		require.NoError(t, err)
		require.Equal(t, []db.Event{snapshot}, published)
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

func checkRejoinRetainedHistoryReset(t *testing.T, store db.Storage, backend Backend, guarded, restored bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%t-%t", guarded, restored))
	require.NoError(t, err)
	issue, old, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	other, otherEvent, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "another task", Author: "old-actor"})
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
	if restored {
		records, err := CollectImportRecords(ctx, store, db.ExportFilter{IncludeDeleted: true})
		require.NoError(t, err)
		store = backend.Open(t)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		binding.Actor = "old-actor"
		binding.RetainHistory = true
		binding, err = store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		require.GreaterOrEqual(t, binding.RetainedThroughEventID, old.ID)
	}
	if guarded {
		err = store.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, snapshot.ID+1, snapshot.ID, store.InstanceUID(), snapshot.ID)
	} else {
		err = store.ResetFederatedProject(ctx, project.ID, snapshot.ID+1, snapshot.ID)
	}
	require.NoError(t, err)
	events, err := store.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
	require.Equal(t, old.OriginInstanceUID, events[0].OriginInstanceUID)
	events, err = store.EventsByUIDs(ctx, project.ID, []string{otherEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, otherEvent.ContentHash, events[0].ContentHash)
	_, err = store.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old task", got.Title)
}

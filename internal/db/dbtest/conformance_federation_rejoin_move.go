package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederationRejoinMoveGate(t *testing.T, store db.Storage, backend Backend) error {
	t.Run("cross-task reply", func(t *testing.T) {
		project, target, source, destination, _, _ := rejoinRetainedReply(t, store, "move-reply")
		for _, issue := range []db.Issue{target, source} {
			current, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			requireRejoinMove(t, store, current, project, destination, db.ErrFederatedMoveUnsupported)
		}
	})
	t.Run("retained history", func(t *testing.T) { require.NoError(t, checkRejoinMoveHistory(t, store)) })
	t.Run("blocks link", func(t *testing.T) { checkRetainedLinkRemoval(t, store, false) })
	t.Run("atomic blocks link", func(t *testing.T) { checkRetainedLinkRemoval(t, store, true) })
	t.Run("accepted recreated link", func(t *testing.T) { require.NoError(t, checkRejoinMoveAcceptedRecreatedLink(t, store)) })
	t.Run("reversed related link", func(t *testing.T) { require.NoError(t, checkRejoinMoveRetainedReversedRelatedLink(t, store)) })
	t.Run("snapshot content", func(t *testing.T) { require.NoError(t, checkRejoinMoveRetainedSnapshotContent(t, store)) })
	t.Run("superseded comment edit", func(t *testing.T) { checkSupersededRetainedCommentEdit(t, store) })
	t.Run("restore from export", func(t *testing.T) { require.NoError(t, checkRestoredRetainedIssueMove(t, store, backend)) })
	return nil
}

func rejoinRetainedReply(t *testing.T, store db.Storage, name string) (db.Project, db.Issue, db.Issue, db.Project, db.Event, db.Event) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, name+"-source")
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, name+"-destination")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, destination)
	target := createHubIssue(t, store, project)
	source := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	comment, root, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "old-actor", Body: "retained"})
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, destination)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, root.ID)
	require.NoError(t, err)
	require.True(t, kept)
	_, reply, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, reply.ID)
	require.NoError(t, err)
	require.True(t, kept)
	return project, target, source, destination, root, reply
}

func checkFederationRejoinCommentReply(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "reply-project")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project)
	target := createHubIssue(t, store, project)
	source := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	comment, root, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "old-actor", Body: "local"})
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, root.ID)
	require.NoError(t, err)
	require.True(t, kept)
	_, reply, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, unrelated, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "unrelated"})
	require.NoError(t, err)
	kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, unrelated.ID)
	require.NoError(t, err)
	require.False(t, kept)
	kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, reply.ID)
	require.NoError(t, err)
	require.True(t, kept)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Contains(t, pending, unrelated)
	require.NotContains(t, pending, reply)
	return nil
}

// bindRejoinSpokes binds projects to one hub as push-enabled spokes of new-actor.
func bindRejoinSpokes(t *testing.T, store db.Storage, projects ...db.Project) {
	t.Helper()
	for i, p := range projects {
		_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: int64(i + 1), HubProjectUID: p.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
	}
}

// pullHubIssueUpdate applies a hub edit to issue, showing the hub has it, and
// returns the issue's current row.
func pullHubIssueUpdate(t *testing.T, store db.Storage, project db.Project, issue db.Issue) db.Issue {
	t.Helper()
	origin, err := uid.New()
	require.NoError(t, err)
	remote := newRemoteEvent(t, project, &issue.UID, "issue.updated", "hub-actor", origin, 500, jsontext.Value(`{"title":"`+issue.Title+`"}`))
	inserted, err := store.InsertRemoteEvent(t.Context(), project.ID, remote)
	require.NoError(t, err)
	require.True(t, inserted)
	current, err := store.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return current
}

func createHubIssue(t *testing.T, store db.Storage, project db.Project) db.Issue {
	t.Helper()
	issueUID, err := uid.New()
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	payload := IssueDocument(t, issueUID, jsontext.Value(`{"uid":"`+issueUID+`","title":"shared","author":"hub-actor","status":"open","metadata":{}}`))
	remote := newRemoteEvent(t, project, &issueUID, "issue.created", "hub-actor", origin, 300, payload)
	inserted, err := store.InsertRemoteEvent(t.Context(), project.ID, remote)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(t.Context(), project.ID))
	issue, err := store.IssueByUID(t.Context(), issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	return issue
}

// requireRejoinMove previews and then commits a move, expecting want from both.
func requireRejoinMove(t *testing.T, store db.Storage, issue db.Issue, from, to db.Project, want error) {
	t.Helper()
	for _, preview := range []bool{true, false} {
		_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: issue.Revision, Actor: "new-actor", DryRun: preview})
		if want == nil {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, want)
		}
	}
}

func checkRejoinMoveAcceptedRecreatedLink(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "recreated-link-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "recreated-link-target")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	issue := createHubIssue(t, store, project)
	peer := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	params := db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "old-actor"}
	input := db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}
	link, retained, err := store.CreateLinkAndEvent(ctx, input, params)
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	params.EventType, params.Actor = "issue.unlinked", "new-actor"
	_, err = store.DeleteLinkAndEvent(ctx, link, params)
	require.NoError(t, err)
	params.EventType = "issue.linked"
	input.Author = "new-actor"
	_, accepted, err := store.CreateLinkAndEvent(ctx, input, params)
	require.NoError(t, err)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
	require.NoError(t, err)
	require.True(t, kept)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, accepted.ID))
	current := pullHubIssueUpdate(t, store, project, issue)
	requireRejoinMove(t, store, current, project, target, nil)
	return nil
}

func checkRejoinMoveRetainedReversedRelatedLink(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "related-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "related-target")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	issue := createHubIssue(t, store, project)
	peer := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, retained, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "related", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: peer.ShortID, FromUID: peer.UID, ToShortID: issue.ShortID, ToUID: issue.UID, Actor: "old-actor"})
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
	require.NoError(t, err)
	require.True(t, kept)
	current := pullHubIssueUpdate(t, store, project, issue)
	requireRejoinMove(t, store, current, project, target, db.ErrFederatedMoveUnsupported)
	return nil
}

func checkRejoinMoveRetainedSnapshotContent(t *testing.T, store db.Storage) error {
	for _, content := range []string{"comment", "link"} {
		t.Run(content, func(t *testing.T) { checkRejoinMoveSnapshotContent(t, store, content) })
	}
	return nil
}

func checkRejoinMoveSnapshotContent(t *testing.T, store db.Storage, content string) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("snapshot-source-%s", content))
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, fmt.Sprintf("snapshot-target-%s", content))
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project)
	issue := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer", Author: "old-actor"})
	require.NoError(t, err)
	if content == "comment" {
		_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "local discussion"})
	} else {
		_, _, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "old-actor"})
	}
	require.NoError(t, err)
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor"})
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	binding := adopted.Binding
	binding.Actor = "new-actor"
	_, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: binding.HubURL, HubProjectID: 2, HubProjectUID: target.UID, Actor: binding.Actor, Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	pending, err := store.PendingFederationPushEvents(ctx, project.ID, store.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	var snapshot db.Event
	for _, event := range pending {
		if event.Type == "issue.snapshot" && event.IssueUID != nil && *event.IssueUID == issue.UID {
			snapshot = event
		}
	}
	require.NotEmpty(t, snapshot.UID)
	if content == "comment" {
		require.Contains(t, snapshot.Payload, "local discussion")
	} else {
		require.Contains(t, snapshot.Payload, peer.UID)
	}
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, snapshot.ID)
	require.NoError(t, err)
	require.True(t, kept)
	current := pullHubIssueUpdate(t, store, adopted.Project, issue)
	requireRejoinMove(t, store, current, adopted.Project, target, db.ErrFederatedMoveUnsupported)
}

func checkRejoinMoveHistory(t *testing.T, store db.Storage) error {
	for _, name := range []string{"retained-root", "dependent-comment", "shared-task", "paused-target", "standalone-target"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			project, err := store.CreateProject(ctx, "history-source-"+name)
			require.NoError(t, err)
			target, err := store.CreateProject(ctx, "history-target-"+name)
			require.NoError(t, err)
			bindRejoinSpokes(t, store, project, target)
			var issue db.Issue
			if name == "dependent-comment" || name == "shared-task" {
				issue = createHubIssue(t, store, project)
			}
			_, err = store.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err)
			var retained db.Event
			if name == "dependent-comment" {
				_, retained, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "local discussion"})
				require.NoError(t, err)
			} else if name != "shared-task" {
				issue, retained, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
				require.NoError(t, err)
			}
			bindRejoinSpokes(t, store, project, target)
			if name != "shared-task" {
				kept, err := store.KeepFederationEventLocal(ctx, project.ID, retained.ID)
				require.NoError(t, err)
				require.True(t, kept)
			}
			if name == "retained-root" {
				_, comment, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "dependent"})
				require.NoError(t, err)
				kept, err := store.KeepFederationDependentLocal(ctx, project.ID, comment.ID)
				require.NoError(t, err)
				require.True(t, kept)
			}
			current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			switch name {
			case "retained-root", "dependent-comment":
				requireRejoinMove(t, store, current, project, target, db.ErrFederatedMoveUnsupported)
			case "shared-task":
				requireRejoinMove(t, store, current, project, target, nil)
			case "paused-target":
				paused, err := store.FederationBindingByProject(ctx, target.ID)
				require.NoError(t, err)
				paused.PushEnabled = false
				_, err = store.UpsertFederationBinding(ctx, paused)
				require.NoError(t, err)
				_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "new-actor", DryRun: true})
				require.ErrorIs(t, err, db.ErrFederatedReadOnly)
				require.NotErrorIs(t, err, db.ErrFederatedMoveUnsupported)
			case "standalone-target":
				_, err = store.LeaveFederationReplica(ctx, project.ID)
				require.NoError(t, err)
				standalone, err := store.CreateProject(ctx, "history-standalone")
				require.NoError(t, err)
				requireRejoinMove(t, store, current, project, standalone, nil)
			}
		})
	}
	return nil
}

func checkRetainedLinkRemoval(t *testing.T, store db.Storage, atomic bool) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, fmt.Sprintf("link-source-%t", atomic))
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, fmt.Sprintf("link-target-%t", atomic))
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	issue := createHubIssue(t, store, project)
	peer := createHubIssue(t, store, project)
	acceptedPeer := createHubIssue(t, store, project)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	params := db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, FromShortID: issue.ShortID, FromUID: issue.UID, ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "old-actor"}
	var link db.Link
	var linkEvent db.Event
	if atomic {
		result, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: issue.ID, Actor: "old-actor", AddBlocks: []int64{peer.ID}})
		require.NoError(t, err)
		require.Len(t, result.Events, 1)
		linkEvent = result.Events[0]
		link, err = store.LinkByEndpoints(ctx, issue.ID, peer.ID, "blocks")
		require.NoError(t, err)
	} else {
		link, linkEvent, err = store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, params)
		require.NoError(t, err)
	}
	bindRejoinSpokes(t, store, project, target)
	acceptedParams := params
	acceptedParams.Actor = "new-actor"
	acceptedParams.ToShortID, acceptedParams.ToUID = acceptedPeer.ShortID, acceptedPeer.UID
	_, acceptedLink, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: acceptedPeer.ID, Type: "blocks", Author: "new-actor"}, acceptedParams)
	require.NoError(t, err)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, linkEvent.ID)
	require.NoError(t, err)
	require.True(t, kept)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, acceptedLink.ID))
	current := pullHubIssueUpdate(t, store, project, issue)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "new-actor", DryRun: true})
	require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
	params.EventType, params.Actor = "issue.unlinked", "new-actor"
	_, err = store.DeleteLinkAndEvent(ctx, link, params)
	require.NoError(t, err)
	current, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	requireRejoinMove(t, store, current, project, target, nil)
}

func checkSupersededRetainedCommentEdit(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "comment-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "comment-target")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project, target)
	issue := createHubIssue(t, store, project)
	comment, created, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "original"})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, created.ID))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, oldEdit, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "old-actor", Body: "local edit"})
	require.NoError(t, err)
	require.True(t, changed)
	bindRejoinSpokes(t, store, project, target)
	kept, err := store.KeepFederationEventLocal(ctx, project.ID, oldEdit.ID)
	require.NoError(t, err)
	require.True(t, kept)
	_, acceptedEdit, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "new-actor", Body: "accepted edit"})
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, acceptedEdit.ID))
	current := pullHubIssueUpdate(t, store, project, issue)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: project.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "new-actor", DryRun: true})
	require.NoError(t, err)
}

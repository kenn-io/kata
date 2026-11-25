package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederationRejoinMoveGate(t *testing.T, store db.Storage) error {
	t.Run("retained project blocks unrelated tasks in both directions", func(t *testing.T) {
		project, _, _, destination, _, _ := rejoinRetainedReply(t, store, "move-project-gate")
		outgoing := createHubIssue(t, store, project)
		incoming := createHubIssue(t, store, destination)
		requireRejoinMove(t, store, outgoing, project, destination, db.ErrFederatedMoveUnsharedHistory)
		requireRejoinMove(t, store, incoming, destination, project, db.ErrFederatedMoveUnsharedHistory)
	})
	t.Run("purging retained history allows moves", func(t *testing.T) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "purged-retained-source")
		require.NoError(t, err)
		destination, err := store.CreateProject(ctx, "purged-retained-destination")
		require.NoError(t, err)
		retained, event, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "old-actor"})
		require.NoError(t, err)
		bindRejoinSpokes(t, store, project, destination)
		kept, err := store.KeepFederationEventLocal(ctx, project.ID, event.ID)
		require.NoError(t, err)
		require.True(t, kept)
		movable := createHubIssue(t, store, project)
		_, err = store.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		_, err = store.PurgeIssue(ctx, retained.ID, "new-actor", nil)
		require.NoError(t, err)
		bindRejoinSpokes(t, store, project)
		requireRejoinMove(t, store, movable, project, destination, nil)
	})
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
	t.Run("snapshot comments", func(t *testing.T) { checkRejoinSnapshotComments(t, store) })
	ctx := t.Context()
	project, target, source, _, _, _ := rejoinRetainedReply(t, store, "reply-project")
	comments, err := store.CommentsByIssue(ctx, target.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	_, edit, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: target.ID, CommentUID: comments[0].UID, Actor: "new-actor", Body: "edited locally"})
	require.NoError(t, err)
	require.True(t, changed)
	kept, err := store.KeepFederationCommentDependentLocal(ctx, project.ID, edit.ID)
	require.NoError(t, err)
	require.True(t, kept)
	origin, err := uid.New()
	require.NoError(t, err)
	proof := newRemoteEvent(t, project, &target.UID, db.RepeatedImportEventType, "hub-actor", origin, 500, IssueDocument(t, target.UID, jsontext.Value(`{"uid":"`+target.UID+`","title":"shared","author":"hub-actor","status":"open","metadata":{},"comments":[{"comment_uid":"`+comments[0].UID+`","author":"old-actor","body":"retained","created_at":"2026-05-23T12:00:00Z"}]}`)))
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, proof)
	require.NoError(t, err)
	require.True(t, inserted)
	_, edit, changed, err = store.EditComment(ctx, db.EditCommentParams{IssueID: target.ID, CommentUID: comments[0].UID, Actor: "new-actor", Body: "edited"})
	require.NoError(t, err)
	require.True(t, changed)
	kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, edit.ID)
	require.NoError(t, err)
	require.False(t, kept, "hub accepted the retained comment")
	_, acceptedReply, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "reply", ReplyToUID: comments[0].UID, ReplyKind: "reply"})
	require.NoError(t, err)
	kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, acceptedReply.ID)
	require.NoError(t, err)
	require.False(t, kept, "hub accepted the retained comment")
	return nil
}

func checkRejoinSnapshotComments(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "snapshot-comments")
	require.NoError(t, err)
	bindRejoinSpokes(t, store, project)
	issue := createHubIssue(t, store, project)
	snapshotCommentUID, err := uid.New()
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	remoteSnapshot := newRemoteEvent(t, project, &issue.UID, "issue.snapshot", "hub-actor", origin, 400, IssueDocument(t, issue.UID, jsontext.Value(`{"uid":"`+issue.UID+`","title":"shared","author":"hub-actor","status":"open","metadata":{},"comments":[{"comment_uid":"`+snapshotCommentUID+`","author":"hub-actor","body":"snapshot shared","created_at":"2026-05-23T12:00:00Z"}]}`)))
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, remoteSnapshot)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	restatedUID, err := uid.New()
	require.NoError(t, err)
	restated := newRemoteEvent(t, project, &issue.UID, db.RepeatedImportEventType, "hub-actor", origin, 403, IssueDocument(t, issue.UID, jsontext.Value(`{"uid":"`+issue.UID+`","title":"shared","author":"hub-actor","status":"open","metadata":{},"comments":[{"comment_uid":"`+restatedUID+`","author":"hub-actor","body":"restated","created_at":"2026-05-23T12:00:00Z"}]}`)))
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, restated)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	sharedUID, err := uid.New()
	require.NoError(t, err)
	sharedEvent := newRemoteEvent(t, project, &issue.UID, "issue.commented", "hub-actor", origin, 401, jsontext.Value(`{"comment_uid":"`+sharedUID+`","author":"hub-actor","body":"shared","created_at":"2026-05-23T12:01:00Z"}`))
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, sharedEvent)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	stubUID, err := uid.New()
	require.NoError(t, err)
	stubEvent := newRemoteEvent(t, project, &issue.UID, "issue.comment_edited", "hub-actor", origin, 402, jsontext.Value(`{"comment_uid":"`+stubUID+`","body":"stub","edited_at":"2026-05-23T12:02:00Z"}`))
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, stubEvent)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	acknowledged, acknowledgedEvent, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "acknowledged"})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, project.ID, acknowledgedEvent.ID))
	laterRestated, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "restated later"})
	require.NoError(t, err)
	remoteRestatement := newRemoteEvent(t, project, &issue.UID, db.RepeatedImportEventType, "hub-actor", origin, 404, IssueDocument(t, issue.UID, jsontext.Value(`{"uid":"`+issue.UID+`","title":"shared","author":"hub-actor","status":"open","metadata":{},"comments":[{"comment_uid":"`+laterRestated.UID+`","author":"hub-actor","body":"restated later","created_at":"2026-05-23T12:00:00Z"}]}`)))
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	local, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "local"})
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor"})
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, binding := rejoinAdopt(t, store, project)
	for _, event := range []db.RemoteEvent{remoteSnapshot, sharedEvent, stubEvent, remoteRestatement} {
		inserted, err = store.InsertRemoteEvent(ctx, project.ID, event)
		require.NoError(t, err)
		require.True(t, inserted)
	}
	keepRejoinSnapshots(t, store, project, binding, issue.UID)
	var kept bool
	for _, comment := range []struct{ name, uid string }{{"local", local.UID}, {"acknowledged", acknowledged.UID}, {"event", sharedUID}, {"snapshot", snapshotCommentUID}, {"stub", stubUID}, {"adopted restatement", restatedUID}, {"restatement", laterRestated.UID}} {
		_, edit, changed, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.uid, Actor: "new-actor", Body: "edited"})
		require.NoError(t, err)
		require.True(t, changed)
		kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, edit.ID)
		require.NoError(t, err)
		require.Equal(t, comment.name == "local", kept, "%s", comment.name)
		_, reply, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: comment.uid, ReplyKind: "reply"})
		require.NoError(t, err)
		kept, err = store.KeepFederationCommentDependentLocal(ctx, project.ID, reply.ID)
		require.NoError(t, err)
		require.Equal(t, comment.name == "local", kept, "%s", comment.name)
	}
}

func bindRejoinSpokes(t *testing.T, store db.Storage, projects ...db.Project) {
	t.Helper()
	for i, p := range projects {
		_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: int64(i + 1), HubProjectUID: p.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
		require.NoError(t, err)
	}
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

func rejoinAdopt(t *testing.T, store db.Storage, project db.Project) (db.Project, db.FederationBinding) {
	t.Helper()
	ctx := t.Context()
	adopted, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "old-actor"})
	require.NoError(t, err)
	_, err = store.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	b := adopted.Binding
	b.Actor = "new-actor"
	_, err = store.UpsertFederationBinding(ctx, b)
	require.NoError(t, err)
	return adopted.Project, b
}

func keepRejoinSnapshots(t *testing.T, store db.Storage, project db.Project, b db.FederationBinding, issueUID string) db.Event {
	t.Helper()
	pending, err := store.PendingFederationPushEvents(t.Context(), project.ID, store.InstanceUID(), b.PushCursorEventID, 100)
	require.NoError(t, err)
	n := 0
	var snapshot db.Event
	for _, e := range pending {
		if e.Type == "issue.snapshot" && e.IssueUID != nil && *e.IssueUID == issueUID {
			kept, err := store.KeepFederationEventLocal(t.Context(), project.ID, e.ID)
			require.NoError(t, err)
			require.True(t, kept)
			n++
			snapshot = e
		}
	}
	require.NotZero(t, n)
	return snapshot
}

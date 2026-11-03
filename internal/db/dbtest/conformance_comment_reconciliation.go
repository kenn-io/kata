package dbtest

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkPayloadOnlySnapshotCommentDisplay(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)

	const issueUID = "01CCCCCCCCCCCCCCCCCCCCCCCC"
	const writerCommentUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	const suppressedCommentUID = "01BBBBBBBBBBBBBBBBBBBBBBBB"
	payload := jsontext.Value(`{"uid":"` + issueUID + `","title":"Task","author":"worker","status":"open","created_at":"2026-10-07T11:00:00Z","comments":[{"comment_uid":"` + writerCommentUID + `","author":"worker","body":"Visible contribution","created_at":"2026-10-07T11:01:00Z","teammate":"route","attributed_teammate":"writer"},{"comment_uid":"` + suppressedCommentUID + `","author":"worker","body":"Explicitly suppressed contribution","created_at":"2026-10-07T11:02:00Z","teammate":"route","attributed_teammate":""}]}`)
	snapshot := newRemoteEvent(t, project, nil, "issue.snapshot", "worker", "01DDDDDDDDDDDDDDDDDDDDDDDD", time.Now().UnixMilli(), payload)
	_, err = store.InsertRemoteEvent(ctx, project.ID, snapshot)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))

	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	byUID := make(map[string]db.Comment, len(comments))
	for _, comment := range comments {
		byUID[comment.UID] = comment
	}
	visible := byUID[writerCommentUID]
	require.NotZero(t, visible.ID)
	require.NotNil(t, visible.AttributedTeammate)
	require.Equal(t, "writer", *visible.AttributedTeammate)
	require.Equal(t, "route", visible.Teammate)
	suppressed := byUID[suppressedCommentUID]
	require.NotZero(t, suppressed.ID)
	require.NotNil(t, suppressed.AttributedTeammate)
	require.Empty(t, *suppressed.AttributedTeammate)
	require.Equal(t, "route", suppressed.Teammate)
	return nil
}

func checkMixedCaseRestoredCommentReconciliation(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	origin := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, origin.Close()) })
	originProject, err := origin.CreateProject(ctx, "origin-project")
	require.NoError(t, err)
	originIssue, _, err := origin.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originProject.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	originTarget, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "worker", Body: "Target", Teammate: "route",
	})
	require.NoError(t, err)
	originOwnedReply, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "worker", Body: "Owned reply", Teammate: "route",
		ReplyToUID: originTarget.UID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	originLocalReply, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: originIssue.ID, Author: "worker", Body: "Local reply", Teammate: "route",
		ReplyToUID: originTarget.UID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	var recordsByUID = make(map[string]*db.CommentExport, 3)
	for record, err := range origin.ExportComments(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		recordsByUID[record.UID] = &exported
	}
	targetRecord := recordsByUID[originTarget.UID]
	ownedReplyRecord := recordsByUID[originOwnedReply.UID]
	localReplyRecord := recordsByUID[originLocalReply.UID]
	require.NotNil(t, targetRecord)
	require.NotNil(t, ownedReplyRecord)
	require.NotNil(t, localReplyRecord)

	lowerTargetUID := strings.ToLower(targetRecord.UID)
	targetRecord.UID = lowerTargetUID
	ownedReplyRecord.ReplyToUID = lowerTargetUID
	ownedReplyRecord.ReplyKind = "reply"
	localReplyRecord.ReplyToUID = lowerTargetUID
	localReplyRecord.ReplyKind = "reply"
	imported := make([]db.ImportRecord, 0, 5)
	for record, err := range origin.ExportProjects(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		imported = append(imported, &exported)
	}
	for record, err := range origin.ExportIssues(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		imported = append(imported, &exported)
	}
	imported = append(imported, targetRecord, ownedReplyRecord, localReplyRecord)
	require.NoError(t, store.ImportReplay(ctx, imported, db.ImportOptions{}))
	project, err := store.ProjectByUID(ctx, originProject.UID)
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, originIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	restored, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, restored, 3)
	restoredByUID := make(map[string]db.Comment, len(restored))
	for _, comment := range restored {
		restoredByUID[comment.UID] = comment
	}
	require.Contains(t, restoredByUID, lowerTargetUID, "the imported row must retain the lowercase ULID that federation will canonicalize")
	require.Equal(t, targetRecord.ID, restoredByUID[lowerTargetUID].ID)
	require.Equal(t, lowerTargetUID, restoredByUID[originOwnedReply.UID].ReplyToUID)
	require.Equal(t, lowerTargetUID, restoredByUID[originLocalReply.UID].ReplyToUID)

	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes", ExternalRootKey: "root-1",
		ExternalAccountKey: "opaque-account-key", Actor: "worker", ReceiveCommentsAfter: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	claimAt := time.Now().UTC()
	claimed, acquired, err := store.ClaimExternalRootBinding(
		ctx, binding.ID, "mixed-case-reconciliation", claimAt, claimAt.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	binding = claimed
	_, err = store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
		BindingID: binding.ID, ClaimToken: binding.ClaimToken, CommentUID: lowerTargetUID, At: claimAt,
	})
	require.NoError(t, err)
	for _, comment := range []struct {
		externalID string
		record     *db.CommentExport
	}{{"external-target", targetRecord}, {"external-reply", ownedReplyRecord}} {
		issueID, commentID := issue.ID, comment.record.ID
		_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: db.ExternalRootCommentMappingSource(binding), ExternalID: comment.externalID,
			ObjectType: "comment", ProjectID: project.ID, IssueID: &issueID, CommentID: &commentID,
		})
		require.NoError(t, err)
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)

	issueUID := issue.UID
	commentPayload := func(record *db.CommentExport, uid, attributed, replyTo, replyKind string) string {
		reply := ""
		if replyTo != "" && replyKind != "" {
			reply = `,"reply_to_uid":"` + replyTo + `","reply_kind":"` + replyKind + `"`
		}
		return `{"comment_uid":"` + uid + `","author":"` + record.Author + `","body":"` + record.Body +
			`","created_at":"` + record.CreatedAt + `","teammate":"` + record.Teammate +
			`","attributed_teammate":"` + attributed + `"` + reply + `}`
	}
	payload := jsontext.Value(`{"uid":"` + issueUID + `","title":"Task","author":"worker","status":"open","created_at":"2026-10-07T11:00:00Z","comments":[` +
		commentPayload(targetRecord, originTarget.UID, "writer", "", "") + `,` +
		commentPayload(ownedReplyRecord, originOwnedReply.UID, "helper", lowerTargetUID, "reply") + `,` +
		commentPayload(localReplyRecord, originLocalReply.UID, "reviewer", lowerTargetUID, "reply") + `]}`)
	snapshot := newRemoteEvent(t, project, &issueUID, "issue.snapshot", "worker", "01DDDDDDDDDDDDDDDDDDDDDDDD", time.Now().UnixMilli(), payload)
	_, err = store.InsertRemoteEvent(ctx, project.ID, snapshot)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	resolvedBinding, err := store.ExternalRootBindingByID(ctx, binding.ID)
	require.NoError(t, err)
	require.Equal(t, originTarget.UID, resolvedBinding.PendingCommentUID,
		"case normalization must keep a pending external comment reference usable")

	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 3)
	byUID := make(map[string]db.Comment, len(comments))
	for _, comment := range comments {
		byUID[comment.UID] = comment
	}
	for _, expected := range []struct {
		uid string
		id  int64
	}{{originTarget.UID, targetRecord.ID}, {originOwnedReply.UID, ownedReplyRecord.ID}, {originLocalReply.UID, localReplyRecord.ID}} {
		comment, ok := byUID[expected.uid]
		require.True(t, ok, "canonical restored comment UID must be present")
		require.Equal(t, expected.id, comment.ID, "reconciliation must preserve the restored comment row ID")
	}
	require.Equal(t, originTarget.UID, byUID[originOwnedReply.UID].ReplyToUID)
	require.Equal(t, "reply", byUID[originOwnedReply.UID].ReplyKind)
	require.Equal(t, originTarget.UID, byUID[originLocalReply.UID].ReplyToUID)
	require.Equal(t, "reply", byUID[originLocalReply.UID].ReplyKind)

	for _, comment := range []struct {
		externalID string
		record     *db.CommentExport
	}{{"external-target", targetRecord}, {"external-reply", ownedReplyRecord}} {
		mapping, err := store.ImportMappingBySource(ctx, project.ID, db.ExternalRootCommentMappingSource(binding), "comment", comment.externalID)
		require.NoError(t, err)
		require.NotNil(t, mapping.CommentID)
		require.Equal(t, comment.record.ID, *mapping.CommentID, "canonical reconciliation must retain external import mappings")
	}

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: issue.ID})
	require.NoError(t, err)
	for _, replyUID := range []string{originOwnedReply.UID, originLocalReply.UID} {
		var reply *db.Comment
		for index := range graph.Comments {
			if graph.Comments[index].Comment.UID == replyUID {
				reply = &graph.Comments[index].Comment
				break
			}
		}
		require.NotNil(t, reply)
		require.Equal(t, originTarget.UID, reply.ReplyToUID)
	}
	return nil
}

func checkFederatedCommentUIDNormalizationPreservesMovedReplies(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	origin := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, origin.Close()) })
	originProject, err := origin.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	targetIssue, _, err := origin.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originProject.ID, Title: "Target", Author: "worker"})
	require.NoError(t, err)
	replyIssue, _, err := origin.CreateIssue(ctx, db.CreateIssueParams{ProjectID: originProject.ID, Title: "Reply", Author: "worker"})
	require.NoError(t, err)
	targetComment, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: targetIssue.ID, Author: "worker", Body: "Target comment",
	})
	require.NoError(t, err)
	replyComment, _, err := origin.CreateComment(ctx, db.CreateCommentParams{
		IssueID: replyIssue.ID, Author: "worker", Body: "Cross-issue reply", ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	var imported []db.ImportRecord
	for record, err := range origin.ExportProjects(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		imported = append(imported, &exported)
	}
	for record, err := range origin.ExportIssues(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		imported = append(imported, &exported)
	}
	recordsByUID := make(map[string]*db.CommentExport, 2)
	for record, err := range origin.ExportComments(ctx, db.ExportFilter{ProjectID: &originProject.ID}) {
		require.NoError(t, err)
		exported := record
		recordsByUID[record.UID] = &exported
	}
	targetRecord := recordsByUID[targetComment.UID]
	replyRecord := recordsByUID[replyComment.UID]
	require.NotNil(t, targetRecord)
	require.NotNil(t, replyRecord)
	lowerTargetUID := strings.ToLower(targetRecord.UID)
	targetRecord.UID = lowerTargetUID
	replyRecord.ReplyToUID = lowerTargetUID
	imported = append(imported, targetRecord, replyRecord)
	require.NoError(t, store.ImportReplay(ctx, imported, db.ImportOptions{}))

	project, err := store.ProjectByUID(ctx, originProject.UID)
	require.NoError(t, err)
	sourceReplyIssue, err := store.IssueByUID(ctx, replyIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	movedTargetIssue, err := store.IssueByUID(ctx, targetIssue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	destination, err := store.CreateProject(ctx, "destination-project")
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: movedTargetIssue.ID, FromProjectID: project.ID, ToProjectID: destination.ID,
		IfMatchRev: movedTargetIssue.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: destination.ID, Role: db.FederationRoleHub,
		HubProjectID: destination.ID, HubProjectUID: destination.UID, Enabled: true,
	})
	require.NoError(t, err)

	issueUID := targetIssue.UID
	payload := jsontext.Value(fmt.Sprintf(
		"{\"uid\":%q,\"title\":%q,\"author\":%q,\"status\":%q,\"created_at\":%q,\"comments\":[{\"comment_uid\":%q,\"author\":%q,\"body\":%q,\"created_at\":%q}]}",
		issueUID, "Target", "worker", "open", movedTargetIssue.CreatedAt.UTC().Format(time.RFC3339Nano),
		targetComment.UID, targetRecord.Author, targetRecord.Body, targetRecord.CreatedAt,
	))
	snapshot := newRemoteEvent(t, destination, &issueUID, "issue.snapshot", "worker", "01DDDDDDDDDDDDDDDDDDDDDDDD", time.Now().UnixMilli(), payload)
	_, err = store.InsertRemoteEvent(ctx, destination.ID, snapshot)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, destination.ID))

	movedComments, err := store.CommentsByIssue(ctx, movedTargetIssue.ID)
	require.NoError(t, err)
	require.Len(t, movedComments, 1)
	require.Equal(t, targetRecord.ID, movedComments[0].ID)
	require.Equal(t, targetComment.UID, movedComments[0].UID)
	replies, err := store.CommentsByIssue(ctx, sourceReplyIssue.ID)
	require.NoError(t, err)
	require.Len(t, replies, 1)
	require.Equal(t, replyRecord.ID, replies[0].ID)
	require.Equal(t, targetComment.UID, replies[0].ReplyToUID,
		"normalizing a moved target must update replies in its former project")

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: sourceReplyIssue.ID})
	require.NoError(t, err)
	target, ok := graph.Targets[targetComment.UID]
	require.True(t, ok)
	require.Equal(t, "moved", target.Status)
	require.NotNil(t, target.Record)
	require.Equal(t, destination.ID, target.Record.ProjectID)
	require.Equal(t, targetRecord.ID, target.Record.Comment.ID)
	return nil
}

func checkFederatedCommentUIDNormalizationResolvesUntouchedTargets(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	const targetProjectUID = "01EEEEEEEEEEEEEEEEEEEEEEEE"
	const targetIssueUID = "01FFFFFFFFFFFFFFFFFFFFFFFF"
	const targetCommentUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	lowerTargetCommentUID := strings.ToLower(targetCommentUID)
	const createdAt = "2026-10-07T11:00:00.000Z"
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{
		&db.ProjectExport{ID: 1, UID: targetProjectUID, Name: "target-project", CreatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1},
		&db.IssueExport{ID: 1, UID: targetIssueUID, ProjectID: 1, ShortID: "ffff", Title: "Target", Status: "open", Author: "worker", CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&db.CommentExport{ID: 1, UID: lowerTargetCommentUID, IssueID: 1, Author: "finder", Body: "Finding", CreatedAt: createdAt},
	}, db.ImportOptions{MergeProject: true}))
	targetProject, err := store.ProjectByUID(ctx, targetProjectUID)
	require.NoError(t, err)

	replyProject, err := store.CreateProject(ctx, "reply-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: replyProject.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 42, HubProjectUID: replyProject.UID, Enabled: true,
	})
	require.NoError(t, err)
	const replyIssueUID = "01CCCCCCCCCCCCCCCCCCCCCCCC"
	const replyUID = "01BBBBBBBBBBBBBBBBBBBBBBBB"
	issueUID := replyIssueUID
	payload := jsontext.Value(fmt.Sprintf(
		`{"uid":%q,"title":"Reply source","author":"worker","status":"open","created_at":%q,"comments":[{"comment_uid":%q,"author":"worker","body":"Response","created_at":%q,"reply_to_uid":%q,"reply_kind":"reply"}]}`,
		replyIssueUID, createdAt, replyUID, createdAt, lowerTargetCommentUID,
	))
	event := newRemoteEvent(t, replyProject, &issueUID, "issue.snapshot", "worker", "01DDDDDDDDDDDDDDDDDDDDDDDD", 1, payload)
	_, err = store.InsertRemoteEvent(ctx, replyProject.ID, event)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, replyProject.ID))

	replyIssue, err := store.IssueByUID(ctx, replyIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	replies, err := store.CommentsByIssue(ctx, replyIssue.ID)
	require.NoError(t, err)
	require.Len(t, replies, 1)
	require.Equal(t, targetCommentUID, replies[0].ReplyToUID)

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: replyProject.ID, SelectedIssueID: replyIssue.ID,
	})
	require.NoError(t, err)
	target, ok := graph.Targets[targetCommentUID]
	require.True(t, ok, "a canonical reply target resolves even when its project was not materialized")
	require.Equal(t, "moved", target.Status)
	require.NotNil(t, target.Record)
	require.Equal(t, targetProject.ID, target.Record.ProjectID)
	require.Equal(t, lowerTargetCommentUID, target.Record.Comment.UID)
	return nil
}

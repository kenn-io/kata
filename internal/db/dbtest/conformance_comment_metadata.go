package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentMetadataMultiIssue(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "notification-project")
	require.NoError(t, err)
	targetIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Target issue", Author: "worker"})
	require.NoError(t, err)
	replyIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Reply issue", Author: "worker"})
	require.NoError(t, err)
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: targetIssue.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	request := jsontext.Value(`{"state":"requested","re":"` + targetComment.UID + `"}`)
	for _, issue := range []db.Issue{targetIssue, replyIssue} {
		patch := map[string]jsontext.Value{"notify.worker": request}
		if issue.ID == targetIssue.ID {
			patch["notify.other"] = request
		}
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: issue.ID, Actor: "worker", Patch: patch,
		})
		require.NoError(t, err)
	}
	beforeTarget, err := store.IssueByID(ctx, targetIssue.ID)
	require.NoError(t, err)
	beforeReply, err := store.IssueByID(ctx, replyIssue.ID)
	require.NoError(t, err)
	beforeEventID, err := store.MaxEventID(ctx)
	require.NoError(t, err)

	var committed []db.Event
	hook := func(_ context.Context, _ *sql.Tx, issue db.Issue, comment db.Comment) ([]db.CommentMetadataUpdate, error) {
		require.Equal(t, replyIssue.ID, issue.ID)
		require.Equal(t, targetComment.UID, comment.ReplyToUID)
		clear := map[string]jsontext.Value{"notify.worker": jsontext.Value("null")}
		return []db.CommentMetadataUpdate{
			{IssueID: replyIssue.ID, Patch: clear},
			{IssueID: targetIssue.ID, Patch: clear},
			{IssueID: targetIssue.ID, Patch: map[string]jsontext.Value{"notify.other": jsontext.Value("null")}},
		}, nil
	}
	createCtx := db.WithCommentMetadataHook(ctx, hook, &committed)
	created, commentEvent, err := store.CreateComment(createCtx, db.CreateCommentParams{
		IssueID: replyIssue.ID, Author: "worker", Body: "Response", ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.Equal(t, targetComment.UID, created.ReplyToUID)
	require.Len(t, committed, 3)
	require.Equal(t, commentEvent.UID, committed[0].UID)
	require.Equal(t, "issue.commented", committed[0].Type)
	require.Equal(t, "issue.metadata_updated", committed[1].Type)
	require.Equal(t, targetIssue.ID, *committed[1].IssueID, "metadata events are ordered by issue ID")
	require.Equal(t, "worker", committed[1].Actor, "metadata events use the attributed comment actor")
	require.Equal(t, "issue.metadata_updated", committed[2].Type)
	require.Equal(t, replyIssue.ID, *committed[2].IssueID)
	require.Equal(t, "worker", committed[2].Actor, "metadata events use the attributed comment actor")

	eventsAfterComment, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: beforeEventID, ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, eventsAfterComment, len(committed))
	for index, expected := range committed {
		require.Equal(t, expected.UID, eventsAfterComment[index].UID, "retained events must match committed database rows")
	}
	afterTarget, err := store.IssueByID(ctx, targetIssue.ID)
	require.NoError(t, err)
	afterReply, err := store.IssueByID(ctx, replyIssue.ID)
	require.NoError(t, err)
	require.Equal(t, beforeTarget.Revision+1, afterTarget.Revision)
	require.Equal(t, beforeReply.Revision+1, afterReply.Revision)
	require.NotContains(t, afterTarget.Metadata, "notify.worker")
	require.NotContains(t, afterTarget.Metadata, "notify.other")
	require.NotContains(t, afterReply.Metadata, "notify.worker")

	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: targetIssue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.worker": request},
	})
	require.NoError(t, err)
	beforeFailureEventID, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	commentsBeforeFailure, err := store.CommentsByIssue(ctx, replyIssue.ID)
	require.NoError(t, err)
	failedEvents := []db.Event{}
	failingHook := func(_ context.Context, _ *sql.Tx, _ db.Issue, _ db.Comment) ([]db.CommentMetadataUpdate, error) {
		return []db.CommentMetadataUpdate{
			{IssueID: targetIssue.ID, Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value("null")}},
			{IssueID: 1 << 60, Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value("null")}},
		}, nil
	}
	_, _, err = store.CreateComment(db.WithCommentMetadataHook(ctx, failingHook, &failedEvents), db.CreateCommentParams{
		IssueID: replyIssue.ID, Author: "worker", Body: "Must roll back", ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.Error(t, err)
	require.Empty(t, failedEvents)
	afterFailureEvents, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: beforeFailureEventID, ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, afterFailureEvents, "comment and metadata events must roll back together")
	commentsAfterFailure, err := store.CommentsByIssue(ctx, replyIssue.ID)
	require.NoError(t, err)
	require.Len(t, commentsAfterFailure, len(commentsBeforeFailure))
	afterFailureTarget, err := store.IssueByID(ctx, targetIssue.ID)
	require.NoError(t, err)
	require.Contains(t, afterFailureTarget.Metadata, "notify.worker", "earlier metadata writes must roll back")
	return nil
}

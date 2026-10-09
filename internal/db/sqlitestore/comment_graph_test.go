package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type commentGraphReader interface {
	ReadCommentGraph(context.Context, db.CommentGraphQuery) (db.CommentGraphData, error)
}

func TestReadCommentGraph_DeletedSourceRetainsReplyEndpointState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestDB(t)

	sourceProject, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	movedProject, err := store.CreateProject(ctx, "moved-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Reply source", Author: "worker"})
	require.NoError(t, err)
	movedTarget, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Moved target", Author: "worker"})
	require.NoError(t, err)
	removedTarget, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Removed target", Author: "worker"})
	require.NoError(t, err)
	movedComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: movedTarget.ID, Author: "worker", Body: "Moved finding"})
	require.NoError(t, err)
	removedComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: removedTarget.ID, Author: "worker", Body: "Removed finding"})
	require.NoError(t, err)
	movedReply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Moved target response", ReplyToUID: movedComment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	removedReply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Removed target response", ReplyToUID: removedComment.UID, ReplyKind: "confirm"})
	require.NoError(t, err)

	currentMoved, err := store.IssueByID(ctx, movedTarget.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: movedTarget.ID, FromProjectID: sourceProject.ID, ToProjectID: movedProject.ID,
		IfMatchRev: currentMoved.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, removedTarget.ID, "worker")
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, source.ID, "worker")
	require.NoError(t, err)

	reader, ok := any(store).(commentGraphReader)
	require.True(t, ok, "storage must expose a transactional comment graph read")
	graph, err := reader.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: sourceProject.ID, IncludeDeletedSourceIssueID: source.ID,
	})
	require.NoError(t, err)

	replies := make(map[string]db.CommentGraphRecord, len(graph.Comments))
	for _, record := range graph.Comments {
		replies[record.Comment.UID] = record
	}
	for _, reply := range []db.Comment{movedReply, removedReply} {
		record, exists := replies[reply.UID]
		require.True(t, exists, "deleted source comment %s must be in the graph", reply.UID)
		require.Equal(t, source.ID, record.Comment.IssueID)
	}
	require.Equal(t, "moved", graph.Targets[movedComment.UID].Status)
	require.NotNil(t, graph.Targets[movedComment.UID].Record)
	require.Equal(t, movedProject.ID, graph.Targets[movedComment.UID].Record.ProjectID)
	require.Equal(t, "removed", graph.Targets[removedComment.UID].Status)
	require.Nil(t, graph.Targets[removedComment.UID].Record)
}

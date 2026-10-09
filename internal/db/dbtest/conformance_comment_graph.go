package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentGraphDeletedSource(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	sourceProject, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	movedProject, err := store.CreateProject(ctx, "moved-project")
	require.NoError(t, err)
	scopeRoot, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Scope root", Author: "worker"})
	require.NoError(t, err)
	otherRoot, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Other root", Author: "worker"})
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: sourceProject.ID, Title: "Reply source", Author: "worker",
		Links: []db.InitialLink{{Type: "parent", ToNumber: scopeRoot.ID}},
	})
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

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
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

	scopedGraph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: sourceProject.ID, IncludeDeletedSourceIssueID: source.ID,
		IssueScope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: sourceProject.UID, RootIssueUID: scopeRoot.UID},
	})
	require.NoError(t, err)
	scopedReplies := 0
	for _, record := range scopedGraph.Comments {
		if record.Comment.IssueID == source.ID {
			scopedReplies++
			require.Empty(t, record.Comment.ReplyToUID, "out-of-scope endpoints must remain redacted")
		}
	}
	require.Equal(t, 2, scopedReplies, "an in-scope soft-deleted source remains readable")

	outOfScopeGraph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{
		ProjectID: sourceProject.ID, IncludeDeletedSourceIssueID: source.ID,
		IssueScope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: sourceProject.UID, RootIssueUID: otherRoot.UID},
	})
	require.NoError(t, err)
	for _, record := range outOfScopeGraph.Comments {
		require.NotEqual(t, source.ID, record.Comment.IssueID, "a deleted source outside the token subtree must stay hidden")
	}

	withoutDeletedSource, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: sourceProject.ID})
	require.NoError(t, err)
	for _, record := range withoutDeletedSource.Comments {
		require.NotEqual(t, source.ID, record.Comment.IssueID, "deleted source comments require explicit selection")
	}
	return nil
}

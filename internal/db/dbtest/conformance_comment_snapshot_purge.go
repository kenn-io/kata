package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentSnapshotPurgedReplyTarget(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()

	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Reply source", Author: "worker",
	})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Reply target", Author: "worker",
	})
	require.NoError(t, err)
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: target.ID, Author: "reviewer", Body: "Finding",
	})
	require.NoError(t, err)
	reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Response",
		ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID})
	require.NoError(t, err)
	require.Equal(t, "removed", graph.Targets[targetComment.UID].Status)

	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	snapshot, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		ProjectUID: project.UID, SelectedIssueUID: source.UID,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.CommentGraph.Comments, 1)
	require.Equal(t, reply.UID, snapshot.CommentGraph.Comments[0].Comment.UID)
	require.Equal(t, "removed", snapshot.CommentGraph.Targets[targetComment.UID].Status,
		"snapshot projection must use reply_to_uid to find the purged target evidence")
	return nil
}

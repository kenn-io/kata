package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentReplyProjectPurgeLineage(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	sourceProject, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	targetProject, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Reply source", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sourceProject.ID, Title: "Reply target", Author: "worker"})
	require.NoError(t, err)
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	_, replyEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Response", ReplyToUID: targetComment.UID, ReplyKind: "confirm",
	})
	require.NoError(t, err)

	currentTarget, err := store.IssueByID(ctx, target.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: target.ID, FromProjectID: sourceProject.ID, ToProjectID: targetProject.ID,
		IfMatchRev: currentTarget.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: targetProject.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: targetProject.ID, Actor: "worker"})
	require.NoError(t, err)

	events, err := store.EventsByUIDs(ctx, sourceProject.ID, []string{replyEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Nil(t, events[0].RelatedIssueID, "purge must detach the numeric target FK")
	require.NotNil(t, events[0].RelatedIssueUID, "purge must retain the portable target UID")
	require.Equal(t, target.UID, *events[0].RelatedIssueUID)
	require.Equal(t, replyEvent.ContentHash, events[0].ContentHash, "retained UID must preserve the source event hash")

	return nil
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentGraphProjectAccess(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	visible, err := store.CreateProject(ctx, "visible-project")
	require.NoError(t, err)
	hidden, err := store.CreateProject(ctx, "hidden-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: visible.ID, Title: "Reply source", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: visible.ID, Title: "Reply target", Author: "worker"})
	require.NoError(t, err)
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Response", ReplyToUID: finding.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: visible.ID, ToProjectID: hidden.ID, IfMatchRev: target.Revision, Actor: "worker"})
	require.NoError(t, err)

	t.Run("hidden candidates", func(t *testing.T) {
		readCtx := db.WithAuthorizedProjects(t.Context(), []string{visible.UID})
		for _, reference := range []*db.CommentGraphReference{nil, {UID: finding.UID}} {
			graph, err := store.ReadCommentGraph(readCtx, db.CommentGraphQuery{ProjectID: hidden.ID, Reference: reference})
			require.NoError(t, err)
			require.Empty(t, graph.Comments)
			require.Empty(t, graph.Targets)
		}
	})
	t.Run("hidden moved endpoint", func(t *testing.T) {
		readCtx := db.WithExposedIssues(db.WithAuthorizedProjects(t.Context(), []string{visible.UID}))
		graph, err := store.ReadCommentGraph(readCtx, db.CommentGraphQuery{ProjectID: visible.ID, SelectedIssueID: source.ID})
		require.NoError(t, err)
		require.Len(t, graph.Comments, 1)
		require.Equal(t, reply.UID, graph.Comments[0].Comment.UID)
		require.Equal(t, db.CommentGraphTarget{Status: "hidden"}, graph.Targets[finding.UID])
		require.NotContains(t, graph.CommentUIDsByProject, hidden.ID)
		require.ElementsMatch(t, []int64{source.ID}, db.ExposedIssues(readCtx))
	})
	t.Run("visible moved endpoint", func(t *testing.T) {
		readCtx := db.WithExposedIssues(db.WithAuthorizedProjects(t.Context(), []string{visible.UID, hidden.UID}))
		graph, err := store.ReadCommentGraph(readCtx, db.CommentGraphQuery{ProjectID: visible.ID, SelectedIssueID: source.ID})
		require.NoError(t, err)
		require.Equal(t, "moved", graph.Targets[finding.UID].Status)
		require.NotNil(t, graph.Targets[finding.UID].Record)
		require.Equal(t, "Finding", graph.Targets[finding.UID].Record.Comment.Body)
		require.ElementsMatch(t, []int64{source.ID, target.ID}, db.ExposedIssues(readCtx))
	})
	t.Run("reference exposure", func(t *testing.T) {
		readCtx := db.WithExposedIssues(db.WithAuthorizedProjects(t.Context(), []string{visible.UID}))
		graph, err := store.ReadCommentGraph(readCtx, db.CommentGraphQuery{ProjectID: visible.ID, Reference: &db.CommentGraphReference{UID: reply.UID}})
		require.NoError(t, err)
		require.Len(t, graph.Comments, 1)
		require.ElementsMatch(t, []int64{source.ID}, db.ExposedIssues(readCtx))
	})
	return nil
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentReadGraph(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Reply issue"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Finding issue"})
	require.NoError(t, err)
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	reply, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	query := db.CommentGraphQuery{ProjectID: p.ID}
	graph, err := store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, graph.Comments, 2)
	query.AllowedIssueIDs = []int64{source.ID}
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, graph.Comments, 1)
	require.Equal(t, reply.UID, graph.Comments[0].Comment.UID)
	require.Empty(t, graph.Comments[0].Comment.ReplyToUID, "hidden target must be redacted before projection")
	query.AllowedIssueIDs = []int64{}
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Empty(t, graph.Comments)
	query.AllowedIssueIDs = nil
	ui, ok := store.(db.UIStore)
	require.True(t, ok)
	snapshot, err := ui.ReadUISnapshot(ctx, db.UISnapshotQuery{ProjectUID: p.UID, SelectedIssueUID: source.UID})
	require.NoError(t, err)
	require.Len(t, snapshot.CommentGraph.Comments, 2, "snapshot retains graph rows under its captured cursor")
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	current, err := store.IssueByID(ctx, target.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: target.ID, FromProjectID: p.ID, ToProjectID: other.ID, IfMatchRev: current.Revision, Actor: "worker"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Len(t, graph.Comments, 1)
	require.Equal(t, "moved", graph.Targets[finding.UID].Status)
	require.Equal(t, target.UID, graph.Targets[finding.UID].Record.IssueUID)
	require.Contains(t, graph.CommentUIDsByProject[other.ID], finding.UID)
	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Equal(t, "removed", graph.Targets[finding.UID].Status)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Pending answer", ReplyToUID: "01AAAAAAAAAAAAAAAAAAAAAAAA", ReplyKind: "reply"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Equal(t, "pending", graph.Targets["01AAAAAAAAAAAAAAAAAAAAAAAA"].Status)
	return nil
}

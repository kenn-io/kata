package dbtest

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"testing"
)

func checkVerificationGraph(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Claim"})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Review"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
	require.NoError(t, err)
	confirm, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "worker", Teammate: "reviewer", Body: "Confirmed", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	refute, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "reviewer", Body: "Disputed", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: "Rebuttal", ReplyToUID: refute.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	read := func(q db.CommentGraphQuery) db.Verification {
		data, err := store.ReadCommentGraph(ctx, q)
		require.NoError(t, err)
		rows := make([]commentref.Record, 0, len(data.Comments))
		for _, r := range data.Comments {
			rows = append(rows, commentref.Record{Comment: r.Comment, IssueUID: r.IssueUID, IssueShortID: r.IssueShortID})
		}
		v := commentref.Evaluate(rows, issue.UID, nil)
		v.Redacted += data.Redacted
		return v
	}
	q := db.CommentGraphQuery{ProjectID: p.ID, VerificationIssueID: issue.ID}
	v := read(q)
	require.Len(t, v.Confirmed, 1)
	require.Empty(t, v.OpenRefutations)
	q.IssueScope = &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: issue.UID}
	v = read(q)
	require.Empty(t, v.Confirmed)
	require.Empty(t, v.OpenRefutations)
	require.Equal(t, 2, v.Redacted)
	_, _, _, err = store.SoftDeleteIssue(ctx, other.ID, "worker")
	require.NoError(t, err)
	q.IssueScope = nil
	v = read(q)
	require.Empty(t, v.Confirmed)
	require.Zero(t, v.Redacted)
	require.NotEmpty(t, confirm.UID)
	return nil
}

package dbtest

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"testing"
	"time"
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

// M1/H1: current-project verdicts follow endpoint moves; retained close
// references remain canonical and respect the reader's current project grant.
func checkVerificationMovesAndHistory(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "claim-project")
	require.NoError(t, err)
	q, err := store.CreateProject(ctx, "review-project")
	require.NoError(t, err)
	claimIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Claim", Author: "worker"})
	require.NoError(t, err)
	reviewIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Review", Author: "reviewer"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: claimIssue.ID, Author: "worker", Body: "Claim"})
	require.NoError(t, err)
	r, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: reviewIssue.ID, Author: "reviewer", Body: "Retained review excerpt", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	read := func() db.Verification {
		data, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: claimIssue.ProjectID})
		require.NoError(t, err)
		rows := []commentref.Record{}
		for _, v := range data.Comments {
			rows = append(rows, commentref.Record{Comment: v.Comment, IssueUID: v.IssueUID, IssueShortID: v.IssueShortID})
		}
		return commentref.Evaluate(rows, claimIssue.UID, nil)
	}
	move := func(issue *db.Issue, to int64) {
		current, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: current.ProjectID, ToProjectID: to, IfMatchRev: current.Revision, Actor: "worker"})
		require.NoError(t, err)
		*issue, err = store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
	}
	require.Len(t, read().OpenRefutations, 1)
	_, events, _, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: claimIssue.ID, ExpectedProjectID: p.ID, Actor: "worker", Reason: "done", Message: "Completed the requested work and recorded the unresolved review.", Evidence: []db.Evidence{{Type: "test", Command: "go test ./example"}}, VerificationIssueUIDs: []string{claimIssue.UID, reviewIssue.UID}, VerificationWarnings: []db.VerificationWarning{{Code: "open_refutations", Refutations: []db.OpenRefutation{{Refutation: r.UID, Target: claim.UID, Author: "reviewer", Summary: r.Body}}}}})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	move(&reviewIssue, q.ID)
	require.Empty(t, read().OpenRefutations)
	restricted := db.WithAuthorizedProjects(ctx, []string{p.UID})
	feed, err := store.EventsAfter(restricted, db.EventsAfterParams{ProjectID: p.ID, IssueUID: claimIssue.UID, Types: []string{"issue.closed"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, feed, 1)
	require.Equal(t, db.ProjectScopeResetEventType, feed[0].Type)
	require.NotContains(t, feed[0].Payload, r.Body)
	move(&reviewIssue, p.ID)
	require.Len(t, read().OpenRefutations, 1)
	feed, err = store.EventsAfter(restricted, db.EventsAfterParams{ProjectID: p.ID, IssueUID: claimIssue.UID, Types: []string{"issue.closed"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, feed, 1)
	require.Contains(t, feed[0].Payload, r.UID)
	move(&claimIssue, q.ID)
	require.Empty(t, read().OpenRefutations)
	move(&reviewIssue, q.ID)
	require.Len(t, read().OpenRefutations, 1)
	_, _, _, err = store.SoftDeleteIssue(ctx, reviewIssue.ID, "reviewer")
	require.NoError(t, err)
	require.Empty(t, read().OpenRefutations)
	return nil
}

func checkVerificationLifecycle(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Claim"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Original completion claim"})
	require.NoError(t, err)
	confirmation, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Independent confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	require.NotNil(t, backend.BackdateCommentCreated)
	require.NoError(t, backend.BackdateCommentCreated(ctx, store, confirmation.ID, time.Now().Add(-time.Hour)))
	read := func() db.Verification {
		data, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: p.ID})
		require.NoError(t, err)
		rows := []commentref.Record{}
		for _, r := range data.Comments {
			rows = append(rows, commentref.Record{Comment: r.Comment, IssueUID: r.IssueUID, IssueShortID: r.IssueShortID})
		}
		return commentref.Evaluate(rows, issue.UID, nil)
	}
	require.Len(t, read().Confirmed, 1)
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: claim.UID, Actor: "worker", Body: "Edited completion claim"})
	require.NoError(t, err)
	require.Empty(t, read().Confirmed)
	fresh, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Teammate: "fresh-review", Body: "Reviewed the edited text", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	require.Len(t, read().Confirmed, 1)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Withdraw this confirmation", ReplyToUID: fresh.UID, ReplyKind: "supersede"})
	require.NoError(t, err)
	require.Empty(t, read().Confirmed)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Unresolved refutation", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	_, _, _, err = store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: claim.UID, Actor: "worker", Body: "Another body edit cannot dismiss this dispute"})
	require.NoError(t, err)
	require.Len(t, read().OpenRefutations, 1)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Replacement claim", ReplyToUID: claim.UID, ReplyKind: "supersede"})
	require.NoError(t, err)
	require.Empty(t, read().OpenRefutations)
	require.Empty(t, read().Confirmed)
	return nil
}

func checkVerificationNestedRedaction(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Claim"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "reviewer", Title: "Review", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	hidden, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Outside reply"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "worker", Body: "Claim"})
	require.NoError(t, err)
	refute, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Author: "reviewer", Body: "Refutation", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	rebuttal, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: hidden.ID, Author: "worker", Body: "Hidden nested rebuttal", ReplyToUID: refute.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	query := db.CommentGraphQuery{ProjectID: p.ID, VerificationIssueID: root.ID, IssueScope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: p.UID, RootIssueUID: root.UID}}
	graph, err := store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Equal(t, 1, graph.Redacted)
	for _, record := range graph.Comments {
		require.NotEqual(t, rebuttal.UID, record.Comment.UID)
	}
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: hidden.ID, ToIssueID: root.ID, Type: "parent", Author: "worker"})
	require.NoError(t, err)
	graph, err = store.ReadCommentGraph(ctx, query)
	require.NoError(t, err)
	require.Zero(t, graph.Redacted)
	return nil
}

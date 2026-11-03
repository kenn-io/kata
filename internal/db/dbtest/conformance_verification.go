package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
)

func checkVerificationSameMillisecond(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Edited claim"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	confirmation, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Confirmed", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	// Both stores retain historical millisecond edit stamps. A confirmation
	// inside that millisecond cannot establish whether it reviewed the edit.
	anchor := time.Date(2026, 10, 7, 12, 0, 0, 123_000_000, time.UTC)
	writer := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = writer.ExecContext(ctx, "UPDATE comments SET edited_at = $1 WHERE id = $2", anchor.Format(time.RFC3339Nano), claim.ID)
	require.NoError(t, err)
	for _, offset := range []time.Duration{0, 500 * time.Microsecond, time.Millisecond} {
		require.NoError(t, backend.BackdateCommentCreated(ctx, store, confirmation.ID, anchor.Add(offset)))
		graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: p.ID})
		require.NoError(t, err)
		rows := make([]commentref.Record, 0, len(graph.Comments))
		for _, record := range graph.Comments {
			rows = append(rows, commentref.Record{Comment: record.Comment, IssueUID: record.IssueUID, IssueShortID: record.IssueShortID})
		}
		v := commentref.Evaluate(rows, issue.UID, nil)
		require.Equal(t, offset == time.Millisecond, len(v.Confirmed) == 1)
	}
	return nil
}

// M6: trusted late replication updates live verdicts without reopening the
// issue or rewriting the evidence accepted by the closing daemon.
func checkVerificationLateReplication(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: p.UID, Enabled: true})
	require.NoError(t, err)
	const issueUID = "01CCCCCCCCCCCCCCCCCCCCCCCC"
	const claimUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	const originUID = "01DDDDDDDDDDDDDDDDDDDDDDDD"
	issueRef := issueUID
	snapshot := newRemoteEvent(t, p, &issueRef, "issue.snapshot", "worker", originUID, 1, jsontext.Value(`{"uid":"`+issueUID+`","title":"Completed task","author":"worker","status":"open","created_at":"2026-10-07T11:00:00Z","comments":[{"comment_uid":"`+claimUID+`","author":"worker","body":"Completion claim","created_at":"2026-10-07T11:00:00Z"},{"comment_uid":"01BBBBBBBBBBBBBBBBBBBBBBBB","author":"reviewer","body":"Confirmed","created_at":"2026-10-07T11:01:00Z","reply_to_uid":"`+claimUID+`","reply_kind":"confirm"}]}`))
	closed := newRemoteEvent(t, p, &issueRef, "issue.closed", "worker", originUID, 2, jsontext.Value(`{"reason":"done","message":"Completed the requested work and checked its focused tests.","closed_at":"2026-10-07T11:02:00Z","evidence":[{"type":"confirmed","comment":"`+claimUID+`"}]}`))
	for _, event := range []db.RemoteEvent{snapshot, closed} {
		_, err = store.InsertRemoteEvent(ctx, p.ID, event)
		require.NoError(t, err)
	}
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	read := func() db.Verification {
		graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: p.ID})
		require.NoError(t, err)
		rows := make([]commentref.Record, 0, len(graph.Comments))
		for _, record := range graph.Comments {
			rows = append(rows, commentref.Record{Comment: record.Comment, IssueUID: record.IssueUID, IssueShortID: record.IssueShortID})
		}
		return commentref.Evaluate(rows, issueUID, nil)
	}
	require.Len(t, read().Confirmed, 1)
	before, err := store.EventsByUIDs(ctx, p.ID, []string{closed.EventUID})
	require.NoError(t, err)
	require.Len(t, before, 1)
	late := newRemoteEvent(t, p, &issueRef, "issue.commented", "second-reviewer", originUID, 3, jsontext.Value(`{"comment_uid":"01EEEEEEEEEEEEEEEEEEEEEEEE","author":"second-reviewer","body":"A late independent refutation","created_at":"2026-10-07T11:03:00Z","reply_to_uid":"`+claimUID+`","reply_kind":"refute"}`))
	_, err = store.InsertRemoteEvent(ctx, p.ID, late)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, p.ID))
	verdict := read()
	require.Empty(t, verdict.Confirmed)
	require.Len(t, verdict.OpenRefutations, 1)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "closed", issue.Status)
	after, err := store.EventsByUIDs(ctx, p.ID, []string{closed.EventUID})
	require.NoError(t, err)
	require.Equal(t, before, after)
	return nil
}

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
	editedClaim, _, _, err := store.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: claim.UID, Actor: "worker", Body: "Edited completion claim"})
	require.NoError(t, err)
	require.Empty(t, read().Confirmed)
	fresh, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Teammate: "fresh-review", Body: "Reviewed the edited text", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	require.NotNil(t, editedClaim.EditedAt)
	require.NoError(t, backend.BackdateCommentCreated(ctx, store, fresh.ID, editedClaim.EditedAt.Add(time.Millisecond)))
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

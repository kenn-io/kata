package dbtest

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkLocalCommentReplies(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	newIssue := func(title string) db.Issue {
		i, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: title})
		require.NoError(t, err)
		return i
	}
	source, target, other := newIssue("Reply issue"), newIssue("Finding issue"), newIssue("Other reply issue")
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	params := db.CreateCommentParams{IssueID: source.ID, Author: "worker", Teammate: "reviewer", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply", ValidateReply: true}
	first, _, err := store.CreateComment(ctx, params)
	require.NoError(t, err)
	params.IssueID = other.ID
	_, _, err = store.CreateComment(ctx, params)
	var duplicate *db.DuplicateCommentReplyError
	require.ErrorAs(t, err, &duplicate)
	if duplicate == nil {
		return errors.New("duplicate reply error must include the existing comment")
	}
	require.Equal(t, first.UID, duplicate.Comment.UID)
	params.Force = true
	_, _, err = store.CreateComment(ctx, params)
	require.NoError(t, err)
	params.Force = false
	params.Teammate = "another-reviewer"
	_, _, err = store.CreateComment(ctx, params)
	require.NoError(t, err)
	params.ReplyKind = "confirm"
	params.Body = strings.Repeat("é", 39)
	_, _, err = store.CreateComment(ctx, params)
	require.Error(t, err)
	params.Body = strings.Repeat("é", 40)
	_, _, err = store.CreateComment(ctx, params)
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, target.ID, "done", "worker", "Completed example finding with supporting evidence", nil)
	require.NoError(t, err)
	params.ReplyKind = "supersede"
	_, _, err = store.CreateComment(ctx, params)
	require.NoError(t, err, "closed target remains valid")
	_, _, _, err = store.CloseIssue(ctx, other.ID, "done", "worker", "Completed example task with supporting evidence", nil)
	require.NoError(t, err)
	params.Force = true
	_, _, err = store.CreateComment(ctx, params)
	require.ErrorIs(t, err, db.ErrCommentReplyClosed)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: "Ordinary comment on a closed issue", ValidateReply: true})
	require.NoError(t, err)
	params.IssueID = source.ID
	params.ReplyToUID = "01AAAAAAAAAAAAAAAAAAAAAAAA"
	_, _, err = store.CreateComment(ctx, params)
	require.ErrorIs(t, err, db.ErrCommentReplyTarget)
	foreign, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	foreignIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: foreign.ID, Author: "worker", Title: "Foreign target"})
	require.NoError(t, err)
	foreignComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: foreignIssue.ID, Author: "finder", Body: "Other finding"})
	require.NoError(t, err)
	params.ReplyToUID = foreignComment.UID
	_, _, err = store.CreateComment(ctx, params)
	require.ErrorIs(t, err, db.ErrCommentReplyTarget)
	_, _, _, err = store.SoftDeleteIssue(ctx, target.ID, "worker")
	require.NoError(t, err)
	params.ReplyToUID = finding.UID
	_, _, err = store.CreateComment(ctx, params)
	require.ErrorIs(t, err, db.ErrCommentReplyTarget)
	return nil
}

func checkConcurrentLocalCommentReplies(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issues := make([]db.Issue, 3)
	for n := range issues {
		issues[n], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Concurrent reply task"})
		require.NoError(t, err)
	}
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issues[0].ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, source := range issues[1:] {
		go func() {
			<-start
			_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Teammate: "reviewer", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply", ValidateReply: true})
			results <- err
		}()
	}
	close(start)
	success, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
			continue
		}
		if _, ok := errors.AsType[*db.DuplicateCommentReplyError](err); ok {
			conflicts++
		} else {
			require.NoError(t, err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, conflicts)
	count := 0
	for _, source := range issues[1:] {
		cs, err := store.CommentsByIssue(ctx, source.ID)
		require.NoError(t, err)
		count += len(cs)
	}
	require.Equal(t, 1, count)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issues[2].ID, Author: "worker", Teammate: "reviewer", Body: "Another answer", ReplyToUID: finding.UID, ReplyKind: "reply", ValidateReply: true, Force: true})
	require.NoError(t, err)
	return nil
}

package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestScopedSSERechecksCloseVerificationEndpoints(t *testing.T) {
	f := newLateReplyTargetFixture(t)
	owners, err := f.store.CommentIssueIDsByUIDs(f.ctx, []string{f.targetUID})
	require.NoError(t, err)
	claim, _, err := f.store.CreateComment(f.ctx, db.CreateCommentParams{IssueID: f.source.ID, Author: "worker-a", Body: "Completion claim"})
	require.NoError(t, err)
	review, _, err := f.store.CreateComment(f.ctx, db.CreateCommentParams{IssueID: owners[f.targetUID], Author: "reviewer", Body: "Retained review must not leave the subtree", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	_, events, _, err := f.store.CloseIssueGuarded(f.ctx, db.CloseIssueParams{
		IssueID: f.source.ID, ExpectedProjectID: f.project.ID, Actor: "worker-a", Reason: "done",
		Message:               "Completed the requested work and retained the unresolved review.",
		Evidence:              []db.Evidence{{Type: "test", Command: "go test ./example"}},
		VerificationIssueUIDs: []string{f.source.UID},
		VerificationWarnings:  []db.VerificationWarning{{Code: "open_refutations", Refutations: []db.OpenRefutation{{Refutation: review.UID, Target: claim.UID, Author: review.Author, Summary: review.Body}}}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	projected, err := projectCloseEvent(f.ctx, f.store, &events[0])
	require.NoError(t, err)
	require.Contains(t, projected.Payload, review.Body)
	require.NoError(t, f.store.DeleteLinkByID(f.ctx, f.parentLinkID))
	visible, err := scopedEventStillVisible(f.ctx, f.store, *projected)
	require.NoError(t, err)
	require.False(t, visible, "SSE must withhold the retained excerpt after its source leaves the subtree")
}

func TestShowDoesNotReuseEvidenceFromAnOlderClose(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	p, err := store.CreateProject(ctx, "claim-project")
	require.NoError(t, err)
	q, err := store.CreateProject(ctx, "review-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "worker", Title: "Completed task"})
	require.NoError(t, err)
	claim, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Earlier completion claim"})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: issue.ID, ExpectedProjectID: p.ID, Actor: "worker", Reason: "done", Message: "Completed the first change and verified the focused tests.", Evidence: []db.Evidence{{Type: "confirmed", Comment: claim.UID}}})
	require.NoError(t, err)
	_, _, _, err = store.ReopenIssue(ctx, issue.ID, "worker")
	require.NoError(t, err)
	review, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Author: "reviewer", Title: "Later review"})
	require.NoError(t, err)
	issue, _, _, err = store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: issue.ID, ExpectedProjectID: p.ID, Actor: "worker", Reason: "done", Message: "Completed the later change and checked its focused tests.", Evidence: []db.Evidence{{Type: "test", Command: "go test ./example"}}, VerificationIssueUIDs: []string{review.UID}})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: review.ID, FromProjectID: p.ID, ToProjectID: q.ID, IfMatchRev: review.Revision, Actor: "reviewer"})
	require.NoError(t, err)
	restricted := db.WithAuthorizedProjects(ctx, []string{p.UID})
	out, err := buildShowIssueResponse(restricted, ServerConfig{DB: store}, issue, false)
	require.NoError(t, err)
	if out.Body.Verification != nil {
		require.Empty(t, out.Body.Verification.Evidence, "an inaccessible latest close cannot reuse the earlier close's claim")
	}
}

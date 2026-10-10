package sqlitestore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// The incremental-ingest contract requires writes to scale with the accepted
// batch, so an unrelated issue update must not rewrite existing comments.
func TestFederationIngestDoesNotRewriteUnchangedComments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := openTestDB(t)
	p := createProject(ctx, t, d, "hub-project")
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "work", Author: "tester"})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "retained comment"})
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, p.ID, "tester")
	require.NoError(t, err)
	// Ephemeral test-local DDL observes actual writes, not implementation text.
	_, err = d.ExecContext(ctx, `CREATE TRIGGER reject_unchanged_comment_update BEFORE UPDATE ON comments
 WHEN NEW.issue_id IS OLD.issue_id AND NEW.author IS OLD.author AND NEW.body IS OLD.body
 AND NEW.created_at IS OLD.created_at AND NEW.teammate IS OLD.teammate
 BEGIN SELECT RAISE(ABORT, 'unchanged comment rewritten'); END`)
	require.NoError(t, err)
	spokeUID := newTestUID(t)
	ev := ingestEventWithPayload(t, p.UID, p.Name, spokeUID, &issue.UID, nil, "issue.updated", 9_000_000_000_000,
		`{"issue_uid":"`+issue.UID+`","title":"updated"}`)
	_, err = d.IngestFederationEvents(ctx, ingestParams(p.ID, spokeUID, ev))
	require.NoError(t, err)
}

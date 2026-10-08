package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestCreateIssueRetriesPostCommitReadWithoutDuplicatingIssue(t *testing.T) { //nolint:paralleltest // swaps package var readCreatedIssue
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)

	originalRead := readCreatedIssue
	readAttempts := 0
	readCreatedIssue = func(ctx context.Context, store *Store, id int64) (db.Issue, error) {
		readAttempts++
		if readAttempts == 1 {
			return db.Issue{}, codedSQLiteErr(sqlite3.SQLITE_BUSY)
		}
		return originalRead(ctx, store, id)
	}
	t.Cleanup(func() { readCreatedIssue = originalRead })

	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "post-commit read retry",
		Author:    "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "post-commit read retry", issue.Title)
	assert.Equal(t, "issue.created", evt.Type)
	assert.Equal(t, 2, readAttempts)

	var issueCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = ?`, p.ID).Scan(&issueCount))
	assert.Equal(t, 1, issueCount)

	var eventCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE project_id = ? AND type = 'issue.created'`, p.ID).Scan(&eventCount))
	assert.Equal(t, 1, eventCount)
}

func TestCreateCommentRetriesPostCommitReadWithoutDuplicatingComment(t *testing.T) { //nolint:paralleltest // swaps package var readCreatedComment
	ctx := context.Background()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	p, err := d.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "comment target",
		Author:    "tester",
	})
	require.NoError(t, err)

	originalRead := readCreatedComment
	readAttempts := 0
	readCreatedComment = func(ctx context.Context, store *Store, id int64) (db.Comment, error) {
		readAttempts++
		if readAttempts == 1 {
			return db.Comment{}, codedSQLiteErr(sqlite3.SQLITE_BUSY)
		}
		return originalRead(ctx, store, id)
	}
	t.Cleanup(func() { readCreatedComment = originalRead })

	comment, evt, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "tester",
		Body:    "post-commit comment read retry",
	})
	require.NoError(t, err)
	assert.Equal(t, "post-commit comment read retry", comment.Body)
	assert.Equal(t, "issue.commented", evt.Type)
	assert.Equal(t, 2, readAttempts)

	var commentCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM comments WHERE issue_id = ?`, issue.ID).Scan(&commentCount))
	assert.Equal(t, 1, commentCount)

	var eventCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE issue_id = ? AND type = 'issue.commented'`, issue.ID).Scan(&eventCount))
	assert.Equal(t, 1, eventCount)
}

func TestCreateCommentRetainsCommittedEventsOnFinalReadFailure(t *testing.T) {
	ctx := t.Context()
	store, e := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, e)
	t.Cleanup(func() { _ = store.Close() })
	p, e := store.CreateProject(ctx, "example-project")
	require.NoError(t, e)
	issue, _, e := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker"})
	require.NoError(t, e)
	original := readCreatedComment
	readCreatedComment = func(context.Context, *Store, int64) (db.Comment, error) {
		return db.Comment{}, fmt.Errorf("response read failed")
	}
	t.Cleanup(func() { readCreatedComment = original })
	var events []db.Event
	_, evt, e := store.CreateComment(db.WithCommentMetadataHook(ctx, func(context.Context, *sql.Tx, db.Issue, db.Comment) (map[string]jsontext.Value, error) {
		return map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(`{"from":"worker","message":"inspect"}`)}, nil
	}, &events), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Finding"})
	require.EqualError(t, e, "response read failed")
	require.Equal(t, "issue.commented", evt.Type)
	require.Len(t, events, 2)
	var count int
	require.NoError(t, store.QueryRowContext(ctx, "SELECT count(*) FROM comments WHERE issue_id=?", issue.ID).Scan(&count))
	require.Equal(t, 1, count)
}

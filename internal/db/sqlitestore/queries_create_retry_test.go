package sqlitestore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestCreateIssueRetriesPostCommitReadWithoutDuplicatingIssue(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
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

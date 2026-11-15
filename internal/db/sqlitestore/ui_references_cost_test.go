package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestUIReferences_PageBudget(t *testing.T) {
	raw, projectID := openPageBudgetDB(t)
	ctx := t.Context()
	_, err := raw.ExecContext(ctx, `UPDATE issues SET owner=printf('owner-%02d',id%10) WHERE project_id=?`, projectID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author)
 SELECT id,printf('label-%02d',id%10),'tester' FROM issues WHERE project_id=?`, projectID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author)
 SELECT id,'recent-label','tester' FROM issues WHERE project_id=? ORDER BY id DESC LIMIT 40`, projectID)
	require.NoError(t, err)
	store := &sqlitestore.Store{DB: raw}
	conn, err := raw.Conn(ctx)
	require.NoError(t, err)
	sqlitePageFetches(t, conn, true)
	require.NoError(t, conn.Close())
	refs, err := store.ReadUIReferences(ctx, db.UIReferencesQuery{Limit: 200})
	require.NoError(t, err)
	require.Len(t, refs.Issues, 200)
	require.Len(t, refs.Owners, 10)
	require.Len(t, refs.Labels, 11)
	conn, err = raw.Conn(ctx)
	require.NoError(t, err)
	count := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("50k issues / all choices: %d page fetches", count)
	// About four times the measured 266 fetches.
	require.LessOrEqual(t, count, 1100, "reference choices must read only the requested page")
}

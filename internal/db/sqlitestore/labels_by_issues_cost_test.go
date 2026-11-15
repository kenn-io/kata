package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestLabelsByIssues_PageBudget(t *testing.T) {
	raw, projectID := openPageBudgetDB(t)
	ctx := t.Context()
	_, err := raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author) SELECT id,'example-label','tester' FROM issues WHERE project_id=?`, projectID)
	require.NoError(t, err)
	store := &sqlitestore.Store{DB: raw}
	ids := make([]int64, 50)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	conn, err := raw.Conn(ctx)
	require.NoError(t, err)
	sqlitePageFetches(t, conn, true)
	require.NoError(t, conn.Close())
	_, err = store.LabelsByIssues(ctx, projectID, ids)
	require.NoError(t, err)
	conn, err = raw.Conn(ctx)
	require.NoError(t, err)
	count := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("50k issues / 50 IDs: %d page fetches", count)
	// Between the measured 106 requested-ID fetches and 360 whole-project fetches.
	require.LessOrEqual(t, count, 230, "label hydration must read requested IDs rather than the whole project")
}

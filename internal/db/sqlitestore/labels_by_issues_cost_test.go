package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestLabelsByIssues_PageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	// Leave planner statistics absent to exercise a store before ANALYZE.
	_, err = raw.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000)
 INSERT INTO issues(uid,project_id,short_id,title,author)
 SELECT printf('%026d',x),?,printf('%08d',x),'ordinary task','tester' FROM n`, p.ID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author) SELECT id,'example-label','tester' FROM issues WHERE project_id=?`, p.ID)
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
	_, err = store.LabelsByIssues(ctx, p.ID, ids)
	require.NoError(t, err)
	conn, err = raw.Conn(ctx)
	require.NoError(t, err)
	count := sqlitePageFetches(t, conn, false)
	require.NoError(t, conn.Close())
	t.Logf("50k issues / 50 IDs: %d page fetches", count)
	// Between the measured 106 requested-ID fetches and 360 whole-project fetches.
	require.LessOrEqual(t, count, 230, "label hydration must read requested IDs rather than the whole project")
}

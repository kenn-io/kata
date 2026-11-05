package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestUIReferences_PageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p, err := d.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	// Leave planner statistics absent, as on a newly populated store.
	_, err = raw.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<50000)
 INSERT INTO issues(uid,project_id,short_id,title,author,owner)
 SELECT printf('%026d',x),?,printf('%08d',x),'ordinary task','tester',printf('owner-%02d',x%10) FROM n`, p.ID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author)
 SELECT id,printf('label-%02d',id%10),'tester' FROM issues WHERE project_id=?`, p.ID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author)
 SELECT id,'recent-label','tester' FROM issues WHERE project_id=? ORDER BY id DESC LIMIT 40`, p.ID)
	require.NoError(t, err)
	store := &sqlitestore.Store{DB: raw}
	for _, tc := range []struct {
		name   string
		budget int
	}{
		// About four to five times the measured 266 and 25 fetches.
		{"all choices", 1100},
		{"issue ordering", 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "issue ordering" {
				_, err := raw.ExecContext(ctx, `UPDATE issues SET owner=NULL`)
				require.NoError(t, err)
				_, err = raw.ExecContext(ctx, `DELETE FROM issue_labels`)
				require.NoError(t, err)
			}
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			refs, err := store.ReadUIReferences(ctx, db.UIReferencesQuery{Limit: 200})
			require.NoError(t, err)
			require.Len(t, refs.Issues, 200)
			if tc.name == "all choices" {
				require.Len(t, refs.Owners, 10)
				require.Len(t, refs.Labels, 11)
			}
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			count := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("50k issues / %s: %d page fetches", tc.name, count)
			require.LessOrEqual(t, count, tc.budget, "reference choices must read only the requested page")
		})
	}
}

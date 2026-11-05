package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

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
	_, err = raw.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
 INSERT INTO issues(uid,project_id,short_id,title,author)
 SELECT printf('%026d',x),?,printf('%08d',x),'ordinary task','tester' FROM n`, p.ID)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `INSERT INTO issue_labels(issue_id,label,author) SELECT id,'example-label','tester' FROM issues WHERE project_id=?`, p.ID)
	require.NoError(t, err)
	store := &sqlitestore.Store{DB: raw}
	t.Run("without-stats", func(t *testing.T) {
		for _, size := range []int{50, 1001} {
			t.Run(fmt.Sprintf("ids=%d", size), func(t *testing.T) {
				ids := make([]int64, size)
				for i := range ids {
					ids[i] = int64(i + 1)
				}
				conn, err := raw.Conn(ctx)
				require.NoError(t, err)
				sqlitePageFetches(t, conn, true)
				require.NoError(t, conn.Close())
				start := time.Now()
				got, err := store.LabelsByIssues(ctx, p.ID, ids)
				elapsed := time.Since(start)
				require.NoError(t, err)
				require.Len(t, got, size)
				for _, id := range ids {
					require.Equal(t, []string{"example-label"}, got[id])
				}
				conn, err = raw.Conn(ctx)
				require.NoError(t, err)
				count := sqlitePageFetches(t, conn, false)
				require.NoError(t, conn.Close())
				t.Logf("100k issues / %d IDs: %d page fetches, %.3f ms", size, count, float64(elapsed)/float64(time.Millisecond))
				// Allow index and issue lookups per requested ID, including chunk boundaries.
				require.LessOrEqual(t, count, size*2+256, "label hydration must read requested IDs rather than the whole project per chunk")
			})
		}
	})
}

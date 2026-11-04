package sqlitestore

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestReadyPriorityUsesOrderedIndexes(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `WITH RECURSIVE numbers(n) AS (
	 SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n < 1000
	) INSERT INTO issues(uid, project_id, short_id, title, author, priority)
	SELECT printf('%026d', n), ?, printf('%06d', n), 'Ready issue', 'author', n % 5 FROM numbers`, project.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, "ANALYZE issues")
	require.NoError(t, err)
	for _, global := range []bool{false, true} {
		query, args := buildReadyIssuesQuery(ctx, project.ID, db.ReadyIssuesFilter{PriorityFirst: true}, time.Now())
		index := "idx_issues_project_ready_priority"
		if global {
			query, args = buildReadyIssuesGlobalQuery(ctx, db.ReadyIssuesFilter{PriorityFirst: true}, time.Now())
			index = "idx_issues_ready_priority"
		}
		rows, err := store.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		require.NoError(t, err)
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			details = append(details, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		plan := strings.Join(details, "\n")
		t.Log(plan)
		require.Contains(t, plan, index)
		require.NotContains(t, plan, "TEMP B-TREE")
	}
}

func BenchmarkReadyPriorityLimitOne(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := b.Context()
			store, err := Open(ctx, filepath.Join(b.TempDir(), "kata.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			project, err := store.CreateProject(ctx, "spoke-project")
			require.NoError(b, err)
			_, err = store.ExecContext(ctx, `WITH RECURSIVE numbers(n) AS (
    SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n < ?
   ) INSERT INTO issues(uid, project_id, short_id, title, author, priority)
   SELECT printf('%026d', n), ?, printf('%06d', n), 'Ready issue', 'author', n % 5 FROM numbers`, count, project.ID)
			require.NoError(b, err)
			_, err = store.ExecContext(ctx, "ANALYZE issues")
			require.NoError(b, err)
			filter := db.ReadyIssuesFilter{PriorityFirst: true}
			issues, err := store.ReadyIssues(ctx, project.ID, 1, filter)
			require.NoError(b, err)
			require.Len(b, issues, 1)
			b.ResetTimer()
			for b.Loop() {
				issues, err := store.ReadyIssues(ctx, project.ID, 1, filter)
				if err != nil || len(issues) != 1 {
					b.Fatalf("ready issues: %v, rows %d", err, len(issues))
				}
			}
		})
	}
}

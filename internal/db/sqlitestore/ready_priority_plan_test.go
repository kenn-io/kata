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
	other, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `WITH RECURSIVE numbers(n) AS (
	 SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n < 1002
	) INSERT INTO issues(uid, project_id, short_id, title, author, priority, owner, updated_at)
SELECT printf('%026d', n), CASE WHEN n <= 1000 THEN ? ELSE ? END, printf('%06d', n), 'Ready issue', 'author', n % 5, CASE WHEN n % 3 = 0 AND n < 1002 THEN NULL ELSE 'other' END, strftime('%Y-%m-%dT%H:%M:%fZ', '2026-01-01', printf('+%d seconds', n)) FROM numbers`, project.ID, other.ID)
	require.NoError(t, err)
	for _, scope := range []string{"project", "global"} {
		t.Run(scope, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				filter db.ReadyIssuesFilter
				wantID int64
			}{
				{name: "all", filter: db.ReadyIssuesFilter{PriorityFirst: true}, wantID: 1000},
				{name: "unowned", filter: db.ReadyIssuesFilter{PriorityFirst: true, Unowned: true}, wantID: 990},
				{name: "updated", wantID: 1002},
				{name: "updated-unowned", filter: db.ReadyIssuesFilter{Unowned: true}, wantID: 999},
			} {
				if scope == "project" && !tc.filter.PriorityFirst {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					query, args := buildReadyIssuesQuery(ctx, project.ID, tc.filter, time.Now())
					index := "idx_issues_project_ready_priority"
					if scope == "global" {
						query, args = buildReadyIssuesGlobalQuery(ctx, tc.filter, time.Now())
						index = "idx_issues_ready_priority"
						if !tc.filter.PriorityFirst {
							index = "idx_issues_ready_updated"
						}
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
					var issue db.Issue
					if scope == "global" {
						issues, err := store.ReadyIssuesGlobal(ctx, 1, tc.filter)
						require.NoError(t, err)
						require.Len(t, issues, 1)
						issue = issues[0].Issue
					} else {
						issues, err := store.ReadyIssues(ctx, project.ID, 1, tc.filter)
						require.NoError(t, err)
						require.Len(t, issues, 1)
						issue = issues[0]
					}
					require.Equal(t, tc.wantID, issue.ID)
				})
			}
		})
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

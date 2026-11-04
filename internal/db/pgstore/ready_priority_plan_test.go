package pgstore

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestReadyPriorityUsesOrderedIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{Schema: "ready_priority_plan", SchemaMode: SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	// The newest unowned issues, 9996 in the project and 10002 globally,
	// have lower priority than the older P0 issues in every case below.
	_, err = store.ExecContext(ctx, `INSERT INTO issues(uid, project_id, short_id, title, author, priority, owner)
 SELECT lpad(n::text, 26, '0'), CASE WHEN n % 7 = 0 THEN $1::bigint ELSE $2::bigint END, lpad(n::text, 6, '0'), 'Ready issue', 'author', n % 5, CASE WHEN n % 3 = 0 THEN NULL ELSE 'worker' END FROM generate_series(1, 10002) n`, project.ID, other.ID)
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, "ANALYZE issues")
	require.NoError(t, err)
	for _, scope := range []string{"project", "global"} {
		t.Run(scope, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				filter        db.ReadyIssuesFilter
				wantID        int64
				wantProjectID int64
			}{
				{name: "all", filter: db.ReadyIssuesFilter{PriorityFirst: true}, wantID: 10000, wantProjectID: 9975},
				{name: "unowned", filter: db.ReadyIssuesFilter{PriorityFirst: true, Unowned: true}, wantID: 9990, wantProjectID: 9975},
			} {
				t.Run(tc.name, func(t *testing.T) {
					query, args := buildReadyIssuesQuery(ctx, project.ID, tc.filter, time.Now())
					index := "idx_issues_project_ready_priority"
					if scope == "global" {
						query, args = buildReadyIssuesGlobalQuery(ctx, tc.filter, time.Now())
						index = "idx_issues_ready_priority"
					}
					rows, err := store.QueryContext(ctx, "EXPLAIN (COSTS OFF) "+query+" LIMIT 1", args...)
					require.NoError(t, err)
					var details []string
					for rows.Next() {
						var detail string
						require.NoError(t, rows.Scan(&detail))
						details = append(details, detail)
					}
					require.NoError(t, rows.Err())
					require.NoError(t, rows.Close())
					plan := strings.Join(details, "\n")
					t.Log(plan)
					require.Contains(t, plan, index)
					require.NotContains(t, plan, "Sort")
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
						tc.wantID = tc.wantProjectID
					}
					require.Equal(t, tc.wantID, issue.ID)
				})
			}
		})
	}
}

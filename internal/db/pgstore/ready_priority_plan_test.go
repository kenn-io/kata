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
	_, err = store.ExecContext(ctx, `INSERT INTO issues(uid, project_id, short_id, title, author, priority)
 SELECT lpad(n::text, 26, '0'), CASE WHEN n % 10 = 0 THEN $1::bigint ELSE $2::bigint END, lpad(n::text, 6, '0'), 'Ready issue', 'author', n % 5 FROM generate_series(1, 10000) n`, project.ID, other.ID)
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
	}
}

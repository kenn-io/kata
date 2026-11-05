package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/uid"
)

type capturedMoveLookup struct {
	store   *pgstore.Store
	queries []capturedMoveLookupQuery
}

type capturedMoveLookupQuery struct {
	query string
	args  []any
}

func (q *capturedMoveLookup) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.queries = append(q.queries, capturedMoveLookupQuery{query: query, args: append([]any(nil), args...)})
	return q.store.QueryRowContext(ctx, query, args...)
}

// Creation checks historical reservations even without a current collision.
// Both move history and reset references must use existing event indexes.
func TestMovedIssueLookupUsesIssueHistoryIndexes(t *testing.T) {
	store, _ := openConformanceStore(t)
	source, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: source.ID, UID: "01ARZ3NDEKTSV4RRFFQ69AAA11", ShortIDOverride: "aaa11",
		Title: "Moved issue", Author: "agent",
	})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: issue.Revision, Actor: "agent",
	})
	require.NoError(t, err)
	seedIndexedMoveHistoryPlan(t, store, source)

	for _, ref := range []string{issue.ShortID, issue.UID, "zzzz"} {
		t.Run(ref, func(t *testing.T) {
			capture := &capturedMoveLookup{store: store}
			found, err := db.MovedIssueUID(t.Context(), capture, source.ID, ref, true)
			if ref == "zzzz" {
				require.ErrorIs(t, err, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, issue.UID, found)
			}

			tx, err := store.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(t.Context(), "SET LOCAL enable_seqscan = off")
			require.NoError(t, err)

			var movePlans, resetPlans int
			for _, captured := range capture.queries {
				if !strings.Contains(captured.query, "events") {
					continue
				}
				rows, err := tx.QueryContext(t.Context(), "EXPLAIN (COSTS OFF) "+captured.query, captured.args...)
				require.NoError(t, err)
				var plan []string
				for rows.Next() {
					var detail string
					require.NoError(t, rows.Scan(&detail))
					plan = append(plan, detail)
				}
				require.NoError(t, rows.Err())
				require.NoError(t, rows.Close())
				joined := strings.Join(plan, "\n")
				if strings.Contains(captured.query, "WHERE e.type='issue.moved'") {
					movePlans++
					index := "idx_events_moved_from"
					if ref == issue.UID {
						// PostgreSQL may prefer the partial move index on source
						// project because it excludes the unrelated event history.
						require.True(t, strings.Contains(joined, "idx_events_issue_uid") || strings.Contains(joined, "idx_events_moved_from"),
							"UID move lookup must use an event index: %s", joined)
					} else {
						require.Contains(t, joined, index, "move history must use its reference index: %s", joined)
					}
				}
				if strings.Contains(captured.query, "sync.reset_required") {
					resetPlans++
					require.Contains(t, joined, "idx_events_reset_from on events reset",
						"reset references must use an event index: %s", joined)
				}
			}
			require.Positive(t, movePlans, "expected to inspect the move-history lookup plan")
			if ref == "zzzz" {
				require.Positive(t, resetPlans, "a move-history miss must inspect the reset-reference lookup plan")
			} else {
				require.Zero(t, resetPlans, "a move-history hit must not inspect reset references")
			}
		})
	}
}

func seedIndexedMoveHistoryPlan(t *testing.T, store *pgstore.Store, project db.Project) {
	t.Helper()
	const eventCount = 2000
	args := make([]any, 0, eventCount*10)
	values := make([]string, 0, eventCount)
	contentHash := strings.Repeat("a", 64)
	for i := range eventCount {
		eventUID, err := uid.New()
		require.NoError(t, err)
		args = append(args, eventUID, store.InstanceUID(), project.ID, project.Name,
			"project.metadata_updated", "agent", `{}`, int64(i+1), int64(0), contentHash)
		first := len(args) - 9
		values = append(values, fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			first, first+1, first+2, first+3, first+4, first+5, first+6, first+7, first+8, first+9))
	}
	_, err := store.ExecContext(t.Context(), `INSERT INTO events(
uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash
) VALUES `+strings.Join(values, ","), args...)
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), "ANALYZE events")
	require.NoError(t, err)
}

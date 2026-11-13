package daemon_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

// Sparse issue history must seek by the header issue UID through the index
// rather than scan unrelated events. The plan is taken from the per-issue page
// query that federation reads run.
func TestIssueHistoryIssueUIDUsesIndex(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
			BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
		})
		_, postgres := store.(*pgstore.Store)
		seed := `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000)
INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT printf('%026d',x),'00000000000000000000000000',?,'shared-project',printf('%026d',x),'issue.closed','member','{}',1,0,printf('%064d',x) FROM n`
		explain := "EXPLAIN QUERY PLAN "
		if postgres {
			seed = `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT lpad(x::text,26,'0'),'00000000000000000000000000',$1,'shared-project',lpad(x::text,26,'0'),'issue.closed','member','{}',1,0,repeat('a',64) FROM generate_series(1,2000) n(x)`
			explain = "EXPLAIN (COSTS OFF) "
		}
		_, err := q.ExecContext(t.Context(), seed, f.public.ID)
		require.NoError(t, err)
		_, err = q.ExecContext(t.Context(), "ANALYZE events")
		require.NoError(t, err)
		tx, err := q.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var plan []string
		_, err = db.ReadFederationEventPage(t.Context(), tx, f.public, f.visible.UID, 0, 100, postgres, func(where string, args ...any) ([]db.Event, error) {
			rows, err := tx.QueryContext(t.Context(), explain+"SELECT e.id FROM events e"+where, args...)
			require.NoError(t, err)
			defer func() { require.NoError(t, rows.Close()) }()
			for rows.Next() {
				var detail string
				if postgres {
					err = rows.Scan(&detail)
				} else {
					var id, parent, unused int
					err = rows.Scan(&id, &parent, &unused, &detail)
				}
				require.NoError(t, err)
				plan = append(plan, detail)
			}
			return nil, rows.Err()
		})
		require.NoError(t, err)
		if postgres {
			require.Contains(t, strings.Join(plan, "\n"), "idx_events_issue_uid_id")
			return
		}
		// A SQLite index ends with the rowid, which is events.id, so both
		// issue_uid indexes seek the issue and its ID range in order.
		require.Regexp(t, `SEARCH e USING INDEX idx_events_issue_uid(_id)? \(issue_uid=\? AND (rowid|id)>\?\)`, strings.Join(plan, "\n"))
	})
}

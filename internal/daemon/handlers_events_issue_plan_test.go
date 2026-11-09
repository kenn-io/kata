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

// Sparse issue history must seek by resolved identity, including payload-only
// federation identities, rather than scan unrelated events.
func TestIssueHistoryResolvedIdentityUsesIndex(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		q := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
			QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		})
		_, postgres := store.(*pgstore.Store)
		seed := `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000)
INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT printf('%026d',x),'00000000000000000000000000',?,'shared-project',printf('%026d',x),'issue.closed','member','{}',1,0,printf('%064d',x) FROM n`
		placeholder, explain := "?", "EXPLAIN QUERY PLAN "
		if postgres {
			seed = `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT lpad(x::text,26,'0'),'00000000000000000000000000',$1,'shared-project',lpad(x::text,26,'0'),'issue.closed','member','{}',1,0,repeat('a',64) FROM generate_series(1,2000) n(x)`
			placeholder, explain = "$1", "EXPLAIN (COSTS OFF) "
		}
		_, err := q.ExecContext(t.Context(), seed, f.public.ID)
		require.NoError(t, err)
		_, err = q.ExecContext(t.Context(), "ANALYZE events")
		require.NoError(t, err)
		rows, err := q.QueryContext(t.Context(), explain+"SELECT e.id FROM events e WHERE e.id > 0 AND "+db.FederationReadIssueUIDExpr(postgres)+" = "+placeholder+" ORDER BY e.id LIMIT 100", f.visible.UID)
		require.NoError(t, err)
		var plan []string
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
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Contains(t, strings.Join(plan, "\n"), "idx_events_resolved_issue_uid")
	})
}

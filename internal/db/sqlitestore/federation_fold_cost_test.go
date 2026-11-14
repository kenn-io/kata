package sqlitestore

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Bulk moves must not make folding each retained event scan every move. Page
// fetches count the work even when SQLite serves the entire history from cache.
func TestFederationFoldBulkMovePageBudget(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 20_000, 2_000)
	raw, err := sql.Open("sqlite", d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			// Each subtest closes its read transaction before statistics change.
			_, err := conn.ExecContext(t.Context(), "ANALYZE")
			require.NoError(t, err)
		}
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
			tx, err := conn.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			cronPullPageFetches(t, conn, true)
			events, err := federationFoldEventsScoped(t.Context(), tx, project.ID, nil, nil)
			require.NoError(t, err)
			pages := cronPullPageFetches(t, conn, false)
			var retained int
			require.NoError(t, tx.QueryRowContext(t.Context(), "SELECT count(*) FROM events WHERE project_id=?", project.ID).Scan(&retained))
			require.Len(t, events, retained+20_000)
			t.Logf("20k history / 2k moves: %d page fetches", pages)
			require.LessOrEqual(t, pages, 120_000, "bulk moves must not multiply history reads by move count")
		})
	}
}

func BenchmarkFederationFoldBulkMoves(b *testing.B) {
	for _, moves := range []int{0, 3_400} {
		for _, group := range []bool{false, true} {
			b.Run(fmt.Sprintf("history=221000/moves=%d/group=%t", moves, group), func(b *testing.B) {
				d, project := seedFederationFoldMoves(b, 221_000, moves)
				projectIDs := []int64{project.ID}
				if group {
					var sourceID int64
					require.NoError(b, d.QueryRowContext(b.Context(), "SELECT id FROM projects WHERE name='source-project'").Scan(&sourceID))
					projectIDs = append(projectIDs, sourceID)
					for i := range 23 {
						p, err := d.CreateProject(b.Context(), fmt.Sprintf("group-project-%d", i))
						require.NoError(b, err)
						projectIDs = append(projectIDs, p.ID)
						_, err = d.ExecContext(b.Context(), `UPDATE events SET project_id=$1, project_name=$2 WHERE project_id=$3 AND id % 25=$4`, p.ID, p.Name, sourceID, i)
						require.NoError(b, err)
					}
				}
				tx, err := d.BeginTx(b.Context(), nil)
				require.NoError(b, err)
				defer func() { require.NoError(b, tx.Rollback()) }()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if group {
						_, err = federationGroupFoldProjection(b.Context(), tx, projectIDs)
					} else {
						_, err = federationFoldEventsScoped(b.Context(), tx, project.ID, nil, nil)
					}
					require.NoError(b, err)
				}
			})
		}
	}
}

// Seed already-materialized destination identities and retained source events,
// avoiding the unrelated cost of creating and replaying thousands of issues.
func seedFederationFoldMoves(t testing.TB, history, moves int) (*Store, db.Project) {
	t.Helper()
	d, destination, _ := incrementalTestStore(t)
	source, err := d.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO issues(uid,short_id,project_id,title,author)
 SELECT printf('%026d',x),printf('%08d',x),$2,'Moved issue','tester' FROM n`, max(moves, 1), destination.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),$2,$3,'source-project',printf('%026d',1+(x-1)%$4),'issue.updated','tester',
 json_object('issue_uid',printf('%026d',1+(x-1)%$4),'title','Retained work'),x,0,$5 FROM n`, history, d.InstanceUID(), source.ID, max(moves, 1), strings.Repeat("a", 64))
	require.NoError(t, err)
	if moves == 0 {
		return d, destination
	}
	_, err = d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<$1)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x+$2),$3,$4,'hub-project',printf('%026d',x),'issue.moved','tester',
 json_object('issue_uid',printf('%026d',x),'from_project_uid',$5,'from_short_id',printf('%08d',x),'to_project_uid',$6,'to_short_id',printf('%08d',x),'updated_at','2026-05-23T12:00:00.000Z'),x+$2,0,$7 FROM n`, moves, history, d.InstanceUID(), destination.ID, source.UID, destination.UID, strings.Repeat("b", 64))
	require.NoError(t, err)
	return d, destination
}

func TestFederationFoldScopedPageBudget(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 20_000, 2_000)
	raw, err := sql.Open("sqlite", d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = conn.ExecContext(t.Context(), "ANALYZE")
			require.NoError(t, err)
		}
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
			tx, err := conn.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			cronPullPageFetches(t, conn, true)
			events, err := federationFoldEventsForIssues(t.Context(), tx, project.ID, []string{"00000000000000000000000001"})
			require.NoError(t, err)
			pages := cronPullPageFetches(t, conn, false)
			var ids []string
			for _, event := range events {
				if strings.HasPrefix(event.UID, "000000000000000000000") {
					ids = append(ids, strings.TrimLeft(event.UID, "0"))
				}
			}
			require.Equal(t, []string{"1", "2001", "4001", "6001", "8001", "10001", "12001", "14001", "16001", "18001", "20001"}, ids)
			t.Logf("%d page fetches", pages)
			require.LessOrEqual(t, pages, 5_000)
		})
	}
}

func TestFederationFoldSelection(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 4, 2)
	const one = "00000000000000000000000001"
	var sourceID int64
	require.NoError(t, d.QueryRowContext(t.Context(), `SELECT id FROM projects WHERE name='source-project'`).Scan(&sourceID))
	for _, row := range []struct {
		id      int
		project int64
		issue   any
	}{{7, project.ID, one}, {8, sourceID, nil}} {
		_, err := d.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash) VALUES(printf('%026d',?),?,?,'test-project',?,'cron.observed','tester',json_object('issue_uid',?),?,0,?)`, row.id, d.InstanceUID(), row.project, row.issue, one, row.id, strings.Repeat("c", 64))
		require.NoError(t, err)
	}
	for _, tc := range []struct {
		name         string
		types, scope []string
		want         []string
		projectEvent bool
	}{
		{"all", nil, nil, []string{"1", "2", "3", "4", "5", "6", "7"}, false},
		{"typed", []string{"issue.updated"}, nil, []string{"1", "2", "3", "4"}, false},
		{"scoped", nil, []string{one}, []string{"1", "3", "5", "7"}, true},
		{"typed-scoped", []string{"issue.updated"}, []string{one, "00000000000000000000000002"}, []string{"1", "2", "3", "4"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := d.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, tx.Rollback()) }()
			events, err := federationFoldEventsScoped(t.Context(), tx, project.ID, tc.types, tc.scope)
			require.NoError(t, err)
			var ids []string
			var projectEvent bool
			for _, event := range events {
				if strings.HasPrefix(event.Type, "project.") {
					projectEvent = true
				}
				if strings.HasPrefix(event.UID, "0000000000000000000000000") {
					ids = append(ids, strings.TrimLeft(event.UID, "0"))
				}
			}
			require.Equal(t, tc.want, ids)
			if tc.projectEvent {
				require.True(t, projectEvent)
			}
		})
	}
}

func TestFederationFoldScopedMovedHistoryPageBudget(t *testing.T) {
	d, project := seedFederationFoldMoves(t, 2_000, 2_000)
	const subject = "00000000000000000000000001"
	_, err := d.ExecContext(t.Context(), `UPDATE events SET issue_uid=?,payload=json_set(payload,'$.issue_uid',?) WHERE type='issue.updated'`, subject, subject)
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	conn, err := raw.Conn(t.Context())
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	cronPullPageFetches(t, conn, true)
	events, err := federationFoldEventsForIssues(t.Context(), tx, project.ID, []string{subject})
	require.NoError(t, err)
	pages := cronPullPageFetches(t, conn, false)
	var ids []string
	for _, event := range events {
		if strings.HasPrefix(event.UID, "0000000000000000000000") {
			ids = append(ids, event.UID)
		}
	}
	require.Len(t, ids, 2_001)
	for i, id := range ids {
		require.Equal(t, fmt.Sprintf("%026d", i+1), id)
	}
	t.Logf("%d page fetches", pages)
	require.LessOrEqual(t, pages, 2_000)
}

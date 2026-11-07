package sqlitestore

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A one-run page must seek its history during both admission and replay.
func TestCronPullRunHistoryPageBudget(t *testing.T) {
	var fetches [][2]int
	for _, count := range []int{1000, 10000} {
		d, p, _ := incrementalTestStore(t)
		ctx := t.Context()
		workflow := cronPullWorkflow(t, p, "workflow", 9_000_001_000_000)
		_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{workflow})
		require.NoError(t, err)
		require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
		run := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FCC", "complete", 9_000_001_000_002)
		_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,?,'cron.run.observed','tester',json_set(?,'$.uid',printf('%026d',x)),9000001000001,0,? FROM n`, count, run.OriginInstanceUID, p.ID, p.Name, string(run.Payload), strings.Repeat("a", 64))
		require.NoError(t, err)
		raw, err := sql.Open("sqlite", d.Path()+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		store := &Store{DB: raw}
		var pages [2]int
		for phase := range 2 {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			cronPullPageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			if phase == 0 {
				_, err = store.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{run})
			} else {
				err = store.MaterializeFederatedEvents(ctx, p.ID, []string{run.EventUID})
			}
			require.NoError(t, err)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			pages[phase] = cronPullPageFetches(t, conn, false)
			require.NoError(t, conn.Close())
		}
		result, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FCC")
		require.NoError(t, err)
		require.Equal(t, "complete", result.Summary.Message)
		t.Logf("history=%d insert=%d materialize=%d page fetches", count, pages[0], pages[1])
		fetches = append(fetches, pages)
	}
	for phase := range 2 {
		require.LessOrEqual(t, fetches[1][phase], fetches[0][phase]+64, "phase %d must skip unrelated run history", phase)
	}
}

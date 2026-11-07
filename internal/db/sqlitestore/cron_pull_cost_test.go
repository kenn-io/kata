package sqlitestore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Both one-run and catch-up pages must seek history during admission and replay.
func TestCronPullRunHistoryPageBudget(t *testing.T) {
	for _, size := range []int{1, 288} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			var fetches [][2]int
			for _, count := range []int{1000, 10000} {
				d, p, _ := incrementalTestStore(t)
				ctx := t.Context()
				workflow := cronPullWorkflow(t, p, "workflow", 9_000_001_000_000)
				_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{workflow})
				require.NoError(t, err)
				tx, err := d.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tx.Rollback() })
				stmt, err := tx.PrepareContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
				require.NoError(t, err)
				for i := range count {
					event := cronPullRun(t, p, workflow, fmt.Sprintf("%026d", i+1), "historical", 9_000_001_000_001+int64(i))
					_, err = stmt.ExecContext(ctx, event.EventUID, event.OriginInstanceUID, p.ID, p.Name, event.Type, event.Actor, string(event.Payload), event.HLCPhysicalMS, event.HLCCounter, event.ContentHash, event.CreatedAt.Format(sqliteTimeFormat))
					require.NoError(t, err)
				}
				require.NoError(t, stmt.Close())
				require.NoError(t, tx.Commit())
				require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
				var materialized int
				require.NoError(t, d.QueryRowContext(ctx, `SELECT count(*) FROM cron_runs WHERE project_id=?`, p.ID).Scan(&materialized))
				require.Equal(t, count, materialized)
				var events []db.RemoteEvent
				var eventUIDs []string
				for i := range size {
					event := cronPullRun(t, p, workflow, fmt.Sprintf("01M6%022d", i+1), "complete", 9_000_002_000_000+int64(i))
					events = append(events, event)
					eventUIDs = append(eventUIDs, event.EventUID)
				}
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
						_, err = store.InsertRemoteEvents(ctx, p.ID, events)
					} else {
						err = store.MaterializeFederatedEvents(ctx, p.ID, eventUIDs)
					}
					require.NoError(t, err)
					conn, err = raw.Conn(ctx)
					require.NoError(t, err)
					pages[phase] = cronPullPageFetches(t, conn, false)
					require.NoError(t, conn.Close())
				}
				for i := range size {
					result, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, fmt.Sprintf("01M6%022d", i+1))
					require.NoError(t, err)
					require.Equal(t, "complete", result.Summary.Message)
				}
				t.Logf("history=%d observations=%d insert=%d materialize=%d page fetches", count, size, pages[0], pages[1])
				fetches = append(fetches, pages)
			}
			for phase := range 2 {
				// A tenfold larger history adds index levels and table-page
				// lookups per observation, rather than a scan of that history.
				require.LessOrEqual(t, fetches[1][phase], fetches[0][phase]+12*size+64, "phase %d must skip unrelated run history", phase)
			}
		})
	}
}

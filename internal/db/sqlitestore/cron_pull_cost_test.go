package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A one-run page must seek its history during both admission and replay.
func TestCronPullRunHistoryPageBudget(t *testing.T) {
	var fetches [][2]int
	for _, count := range []int{1000, 10000} {
		d, path := openTestDBWithPath(t)
		ctx := t.Context()
		p := createProject(ctx, t, d, "spoke-project")
		_, err := d.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
		workflow := db.RemoteEvent{EventUID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", OriginInstanceUID: d.InstanceUID(), ProjectUID: p.UID, ProjectName: p.Name, Type: "cron.workflow.created", Actor: "tester", HLCPhysicalMS: 1000, CreatedAt: time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)}
		workflow.Payload = []byte(fmt.Sprintf(`{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","project_uid":%q,"name":"workflow","author":"tester","created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:00Z","definition":{"version":1,"steps":[{"key":"review","kind":"command","command":"echo synthetic"}]}}`, p.UID))
		workflow.ContentHash, err = db.RemoteEventContentHash(workflow)
		require.NoError(t, err)
		_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{workflow})
		require.NoError(t, err)
		require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
		payload := fmt.Sprintf(`{"project_uid":%q,"uid":"01ARZ3NDEKTSV4RRFFQ69G5FCC","workflow_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAB","workflow_definition_event_uid":%q,"actor":"tester","status":"succeeded","summary":{"version":1,"message":"complete"},"revision":1,"created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:01Z"}`, p.UID, workflow.EventUID)
		_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'spoke-project','cron.run.observed','tester',json_set(?,'$.uid',printf('%026d',x)),1001,0,? FROM n`, count, d.InstanceUID(), p.ID, payload, strings.Repeat("a", 64))
		require.NoError(t, err)
		run := workflow
		run.EventUID = "01ARZ3NDEKTSV4RRFFQ69G5FDD"
		run.Type = "cron.run.observed"
		run.HLCPhysicalMS = 1002
		run.Payload = []byte(payload)
		run.ContentHash, err = db.RemoteEventContentHash(run)
		require.NoError(t, err)
		raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		store := &sqlitestore.Store{DB: raw}
		var pages [2]int
		for phase := range 2 {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			if phase == 0 {
				_, err = store.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{run})
			} else {
				err = store.MaterializeFederatedEvents(ctx, p.ID, []string{run.EventUID})
			}
			require.NoError(t, err)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			pages[phase] = sqlitePageFetches(t, conn, false)
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

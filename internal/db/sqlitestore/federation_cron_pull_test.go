package sqlitestore

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
	"modernc.org/sqlite"
)

func cronPullWorkflow(t testing.TB, p db.Project, name string, clock int64) db.RemoteEvent {
	t.Helper()
	return incrementalRemoteEvent(t, p, "", "cron.workflow.created", fmt.Sprintf(`{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","project_uid":%q,"name":%q,"author":"tester","created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:00Z","definition":{"version":1,"steps":[{"key":"review","kind":"command","command":"echo synthetic"}]}}`, p.UID, name), clock)
}

func cronPullRun(t testing.TB, p db.Project, workflow db.RemoteEvent, uid, message string, clock int64) db.RemoteEvent {
	t.Helper()
	at := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.NewCronRunObservation(db.CronRun{
		UID: uid, WorkflowUID: new("01ARZ3NDEKTSV4RRFFQ69G5FAA"), WorkflowDefinitionEventUID: &workflow.EventUID,
		Actor: "tester", Status: "succeeded", Summary: cron.Summary{Version: 1, Message: message}, Revision: 1,
		CreatedAt: at, UpdatedAt: at.Add(time.Second),
	}, p.UID))
	require.NoError(t, err)
	return incrementalRemoteEvent(t, p, "", "cron.run.observed", string(payload), clock)
}

func TestCronPullMaterializesOnlyTouchedRuns(t *testing.T) {
	d, p, _ := incrementalTestStore(t)
	ctx := t.Context()
	workflow := cronPullWorkflow(t, p, "workflow", 9_000_001_000_000)
	a := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FBB", "original A", 9_000_001_000_001)
	b := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FCC", "original B", 9_000_001_000_001)
	_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{workflow, a, b})
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	before, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FCC")
	require.NoError(t, err)
	// Admission can retain an observation before its pull page is materialized.
	pendingB := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FCC", "pending B", 9_000_001_000_005)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{pendingB})
	require.NoError(t, err)
	later := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FBB", "later A", 9_000_001_000_003)
	older := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FBB", "older A", 9_000_001_000_002)
	c := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FDD", "new C", 9_000_001_000_004)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{c, later, older})
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{c.EventUID, later.EventUID, older.EventUID}))
	after, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FCC")
	require.NoError(t, err)
	require.Equal(t, before, after, "a pull must leave runs outside its page unchanged")
	// A page carrying only an older observation must keep the historical winner.
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{older.EventUID}))
	run, err := (db.CronSQL{Query: d}).Run(ctx, p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FBB")
	require.NoError(t, err)
	require.Equal(t, "later A", run.Summary.Message)
}

func seedCronPullHistory(t testing.TB, d *Store, p db.Project, count int) {
	t.Helper()
	tx, err := d.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	validator := db.NewCronReplayValidator(tx, false)
	var uid string
	for i := range count {
		typ := "issue.updated"
		payload := ""
		if i%50 == 0 {
			uid, err = katauid.New()
			require.NoError(t, err)
			typ = "issue.snapshot"
			payload = fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"history work","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z"}`, uid, strings.ToLower(uid[len(uid)-8:]))
		} else {
			payload = fmt.Sprintf(`{"issue_uid":%q,"title":"history work"}`, uid)
		}
		inserted, err := insertFederationEventTx(t.Context(), tx, p.ID, p.Name, incrementalRemoteEvent(t, p, uid, typ, payload, 9_000_000_000_000+int64(i)), validator)
		require.NoError(t, err)
		require.True(t, inserted)
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, d.MaterializeFederatedProject(t.Context(), p.ID))
}

func cronPullPageFetches(t testing.TB, conn *sql.Conn, reset bool) int {
	t.Helper()
	total := 0
	require.NoError(t, conn.Raw(func(raw any) error {
		for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
			n, _, err := raw.(sqlite.DBStatus).Status(op, reset)
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	}))
	return total
}

// One cron page must not read the unrelated issue history or reconcile its IDs.
// Counting SQLite page fetches avoids scheduler-dependent timing assertions.
func TestCronPullWorkIndependentOfIssueHistory(t *testing.T) {
	var fetches []int
	for _, count := range []int{100, 10_000} {
		d, p, _ := incrementalTestStore(t)
		seedCronPullHistory(t, d, p, count)
		ev := cronPullWorkflow(t, p, "workflow", 9_000_001_000_000)
		_, err := d.InsertRemoteEvents(t.Context(), p.ID, []db.RemoteEvent{ev})
		require.NoError(t, err)
		raw, err := sql.Open("sqlite", d.Path()+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		store := &Store{DB: raw}
		conn, err := raw.Conn(t.Context())
		require.NoError(t, err)
		cronPullPageFetches(t, conn, true)
		require.NoError(t, conn.Close())
		start := time.Now()
		require.NoError(t, store.MaterializeFederatedEvents(t.Context(), p.ID, []string{ev.EventUID}))
		elapsed := time.Since(start)
		conn, err = raw.Conn(t.Context())
		require.NoError(t, err)
		n := cronPullPageFetches(t, conn, false)
		require.NoError(t, conn.Close())
		t.Logf("history=%d pull=%s page_fetches=%d", count, elapsed, n)
		fetches = append(fetches, n)
		var name string
		require.NoError(t, d.QueryRowContext(t.Context(), `SELECT name FROM cron_workflows WHERE project_id=?`, p.ID).Scan(&name))
		require.Equal(t, "workflow", name)
	}
	require.LessOrEqual(t, fetches[1], fetches[0]+100, "unrelated issue history must not grow cron pull work")
}

func BenchmarkCronOnlyPullHistory(b *testing.B) {
	for _, count := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("history=%d", count), func(b *testing.B) {
			d, p, _ := incrementalTestStore(b)
			seedCronPullHistory(b, d, p, count)
			ev := cronPullWorkflow(b, p, "workflow", 9_000_001_000_000)
			_, err := d.InsertRemoteEvents(b.Context(), p.ID, []db.RemoteEvent{ev})
			require.NoError(b, err)
			b.ResetTimer()
			for range b.N {
				require.NoError(b, d.MaterializeFederatedEvents(b.Context(), p.ID, []string{ev.EventUID}))
			}
		})
	}
}

// Mixed pages must still materialize issue deltas, and reset baselines must
// rebuild identities even when cron definitions arrive on an earlier page.
func TestCronPullMatchesFullFold(t *testing.T) {
	for _, scenario := range []string{"cron-only", "runs", "mixed", "reset-cron-first", "move-and-reset-alias"} {
		t.Run(scenario, func(t *testing.T) {
			d, p, issue := incrementalTestStore(t)
			ctx := t.Context()
			_, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "retained comment"})
			require.NoError(t, err)
			require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
			var target db.Project
			var aliases []string
			if scenario == "move-and-reset-alias" {
				target, err = d.CreateProject(ctx, "target-project")
				require.NoError(t, err)
				_, err = d.EnableProjectFederation(ctx, target.ID, "tester")
				require.NoError(t, err)
				current, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: p.ID, ToProjectID: target.ID, IfMatchRev: current.Revision, Actor: "tester"})
				require.NoError(t, err)
				var moveUID string
				require.NoError(t, d.QueryRowContext(ctx, `SELECT uid FROM events WHERE type='issue.moved' AND issue_uid=? ORDER BY id DESC LIMIT 1`, issue.UID).Scan(&moveUID))
				localAlias := strings.ToLower(issue.UID[len(issue.UID)-8:])
				aliases = []string{issue.ShortID, localAlias}
				tx, err := d.BeginTx(ctx, nil)
				require.NoError(t, err)
				_, err = d.insertEventTx(ctx, tx, eventInsert{ProjectID: p.ID, ProjectUID: p.UID, ProjectName: p.Name, IssueUID: &issue.UID, Type: db.ProjectScopeResetEventType, Actor: "system", Payload: fmt.Sprintf(`{"from_project_uid":%q,"from_short_id":%q,"move_event_uid":%q}`, p.UID, localAlias, moveUID), SkipRelay: true})
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
				require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
			}
			if scenario == "reset-cron-first" {
				require.NoError(t, d.ResetFederatedProject(ctx, p.ID, 1, 1))
			}
			reference := cloneIncrementalStore(t, d)
			workflow := cronPullWorkflow(t, p, "workflow", 9_000_001_000_000)
			pages := [][]db.RemoteEvent{{workflow}}
			if scenario == "runs" {
				a := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FBB", "original A", 9_000_001_000_001)
				later := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FBB", "later A", 9_000_001_000_002)
				b := cronPullRun(t, p, workflow, "01ARZ3NDEKTSV4RRFFQ69G5FCC", "new B", 9_000_001_000_003)
				pages = [][]db.RemoteEvent{{workflow, a}, {later, b}}
			}
			if scenario == "mixed" {
				pages[0] = append(pages[0], incrementalRemoteEvent(t, p, "", "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":"mixed update"}`, issue.UID), 9_000_001_000_001))
			}
			if scenario == "reset-cron-first" {
				snapshot := incrementalRemoteEvent(t, p, issue.UID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"reset baseline","author":"tester","status":"open","created_at":"2026-05-23T12:00:00.000Z"}`, issue.UID, issue.ShortID), 9_000_001_000_001)
				pages = append(pages, []db.RemoteEvent{snapshot})
			}
			for _, page := range pages {
				var uids []string
				for _, ev := range page {
					uids = append(uids, ev.EventUID)
				}
				for _, store := range []*Store{d, reference} {
					_, err := store.InsertRemoteEvents(ctx, p.ID, page)
					require.NoError(t, err)
				}
				require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, uids))
				require.NoError(t, reference.MaterializeFederatedProject(ctx, p.ID))
				require.Equal(t, federationProjectionRows(t, reference, p.ID), federationProjectionRows(t, d, p.ID))
				require.Equal(t, cronPullRows(t, reference, p.ID), cronPullRows(t, d, p.ID))
			}
			if scenario == "mixed" || scenario == "reset-cron-first" {
				got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
				require.NoError(t, err)
				want := "mixed update"
				if scenario == "reset-cron-first" {
					want = "reset baseline"
				}
				require.Equal(t, want, got.Title)
				require.Equal(t, issue.ShortID, got.ShortID)
			}
			for _, alias := range aliases {
				uid, err := db.MovedIssueUID(ctx, d, p.ID, alias, false)
				require.NoError(t, err)
				require.Equal(t, issue.UID, uid)
			}
			if target.ID != 0 {
				require.Equal(t, federationProjectionRows(t, reference, target.ID), federationProjectionRows(t, d, target.ID))
			}
		})
	}
}

// Compare portable cron state; local row IDs and replay revision counters can
// legitimately differ with page order. The full-fold reference is independent
// of the scoped preparation, and literal winner assertions guard both paths.
func cronPullRows(t testing.TB, d *Store, projectID int64) [][][]any {
	t.Helper()
	var tables [][][]any
	for _, query := range []string{
		`SELECT uid,name,definition_json,definition_event_uid,definition_hlc_json,author,created_at,updated_at,deleted_at FROM cron_workflows WHERE project_id=? ORDER BY uid`,
		`SELECT uid,name,definition_json,definition_event_uid,definition_hlc_json,author,created_at,updated_at,deleted_at FROM cron_jobs WHERE project_id=? ORDER BY uid`,
		`SELECT uid,workflow_uid,workflow_definition_event_uid,issue_uid,actor,status,summary_json,created_at,updated_at FROM cron_runs WHERE project_id=? ORDER BY uid`,
	} {
		rows, err := d.QueryContext(t.Context(), query, projectID)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		var table [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			require.NoError(t, rows.Scan(targets...))
			table = append(table, values)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		tables = append(tables, table)
	}
	return tables
}

func TestCronPullConvergesAcrossSpokes(t *testing.T) {
	d, p, issue := incrementalTestStore(t)
	require.NoError(t, d.MaterializeFederatedProject(t.Context(), p.ID))
	second := cloneIncrementalStore(t, d)
	second.instanceUID = "01ARZ3NDEKTSV4RRFFQ69G5FCC"
	_, err := second.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key='instance_uid'`, second.instanceUID)
	require.NoError(t, err)
	reference := cloneIncrementalStore(t, d)
	created := cronPullWorkflow(t, p, "created", 9_000_001_000_000)
	updated := incrementalRemoteEvent(t, p, "", "cron.workflow.updated", strings.ReplaceAll(string(cronPullWorkflow(t, p, "winner", 9_000_001_000_002).Payload), `"author":"tester"`, `"author":"editor"`), 9_000_001_000_002)
	stale := incrementalRemoteEvent(t, p, "", "cron.workflow.updated", string(cronPullWorkflow(t, p, "stale", 9_000_001_000_001).Payload), 9_000_001_000_001)
	observation := incrementalRemoteEvent(t, p, "", "cron.run.observed", fmt.Sprintf(`{"project_uid":%q,"uid":"01ARZ3NDEKTSV4RRFFQ69G5FBB","workflow_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","workflow_definition_event_uid":%q,"issue_uid":%q,"actor":"tester","status":"succeeded","summary":{"version":1,"message":"synthetic"},"revision":1,"created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:01Z"}`, p.UID, created.EventUID, issue.UID), 9_000_001_000_003)
	for i, store := range []*Store{d, second, reference} {
		order := []db.RemoteEvent{created, updated, stale, observation}
		if i == 1 {
			order = []db.RemoteEvent{stale, created, observation, updated}
		}
		for _, ev := range order {
			_, err := store.InsertRemoteEvents(t.Context(), p.ID, []db.RemoteEvent{ev})
			require.NoError(t, err)
			if i == 2 {
				require.NoError(t, store.MaterializeFederatedProject(t.Context(), p.ID))
			} else {
				require.NoError(t, store.MaterializeFederatedEvents(t.Context(), p.ID, []string{ev.EventUID}))
			}
		}
		workflow, err := (db.CronSQL{Query: store}).Workflow(t.Context(), p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
		require.NoError(t, err)
		require.Equal(t, "winner", workflow.Name)
		require.Equal(t, "tester", workflow.Author)
		require.Equal(t, updated.EventUID, workflow.DefinitionEventUID)
		run, err := (db.CronSQL{Query: store}).Run(t.Context(), p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FBB")
		require.NoError(t, err)
		require.Equal(t, "succeeded", run.Status)
		require.Equal(t, &issue.UID, run.IssueUID)
	}
	require.Equal(t, cronPullRows(t, reference, p.ID), cronPullRows(t, d, p.ID))
	require.Equal(t, cronPullRows(t, d, p.ID), cronPullRows(t, second, p.ID))
	require.Equal(t, federationProjectionRows(t, d, p.ID), federationProjectionRows(t, second, p.ID))
}

// A newer snapshot envelope must not beat a later edit's original definition
// clock, and job tombstones must survive the same cron-only preparation.
func TestCronPullSnapshotsAndTombstones(t *testing.T) {
	d, p, _ := incrementalTestStore(t)
	require.NoError(t, d.MaterializeFederatedProject(t.Context(), p.ID))
	reference := cloneIncrementalStore(t, d)
	created := cronPullWorkflow(t, p, "created", 9_000_001_000_000)
	updated := incrementalRemoteEvent(t, p, "", "cron.workflow.updated", string(cronPullWorkflow(t, p, "winner", 9_000_001_000_002).Payload), 9_000_001_000_002)
	var baseline db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal(created.Payload, &baseline))
	baseline.DefinitionEventUID = created.EventUID
	baseline.DefinitionHLC = &db.CronDefinitionHLC{Version: 1, PhysicalMS: created.HLCPhysicalMS, OriginInstanceUID: created.OriginInstanceUID}
	payload, err := json.Marshal(baseline)
	require.NoError(t, err)
	snapshot := incrementalRemoteEvent(t, p, "", "cron.workflow.snapshot", string(payload), 9_000_001_000_010)
	jobPayload := fmt.Sprintf(`{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FDD","project_uid":%q,"name":"job","author":"tester","created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:00Z","definition":{"version":1,"kind":"job","enabled":false,"trigger":{"kind":"interval","interval_seconds":60},"action":{"kind":"execute","prompt":"synthetic"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}}`, p.UID)
	job := incrementalRemoteEvent(t, p, "", "cron.job.created", jobPayload, 9_000_001_000_000)
	baseline = db.CronDefinitionEvent{}
	require.NoError(t, json.Unmarshal(job.Payload, &baseline))
	deletedAt := time.Date(2026, 5, 23, 12, 0, 1, 0, time.UTC)
	baseline.DeletedAt = &deletedAt
	payload, err = json.Marshal(baseline)
	require.NoError(t, err)
	deleted := incrementalRemoteEvent(t, p, "", "cron.job.deleted", string(payload), 9_000_001_000_004)
	for _, event := range []db.RemoteEvent{created, updated, snapshot, job, deleted} {
		for _, store := range []*Store{d, reference} {
			_, err := store.InsertRemoteEvents(t.Context(), p.ID, []db.RemoteEvent{event})
			require.NoError(t, err)
		}
		require.NoError(t, d.MaterializeFederatedEvents(t.Context(), p.ID, []string{event.EventUID}))
		require.NoError(t, reference.MaterializeFederatedProject(t.Context(), p.ID))
		require.Equal(t, cronPullRows(t, reference, p.ID), cronPullRows(t, d, p.ID))
	}
	workflow, err := (db.CronSQL{Query: d}).Workflow(t.Context(), p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FAA")
	require.NoError(t, err)
	require.Equal(t, "winner", workflow.Name)
	require.Equal(t, updated.EventUID, workflow.DefinitionEventUID)
	storedJob, err := (db.CronSQL{Query: d}).Job(t.Context(), p.ID, "01ARZ3NDEKTSV4RRFFQ69G5FDD")
	require.NoError(t, err)
	require.NotNil(t, storedJob.DeletedAt)
	require.Equal(t, deleted.EventUID, storedJob.DefinitionEventUID)
}

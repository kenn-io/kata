package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"modernc.org/sqlite"
)

// The observer delegates SQL to real SQLite and counts consumed rows, rather
// than guessing cost from query text or measuring scheduler-sensitive time.
type referenceWork struct{ queries, rows int }
type referenceConnector struct{ work *referenceWork }

func (c referenceConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c referenceConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(":memory:")
	if err != nil {
		return nil, err
	}
	return &referenceConnection{Conn: conn, work: c.work}, nil
}

type referenceConnection struct {
	driver.Conn
	work *referenceWork
}

func (c *referenceConnection) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	c.work.queries++
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, a)
	if err != nil {
		return nil, err
	}
	return &referenceRows{Rows: r, work: c.work}, nil
}
func (c *referenceConnection) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, a)
}

type referenceRows struct {
	driver.Rows
	work *referenceWork
}

func (r *referenceRows) Next(v []driver.Value) error {
	err := r.Rows.Next(v)
	if err == nil {
		r.work.rows++
	}
	return err
}

func referenceWorkFixture(t *testing.T, history int) (*sql.Tx, *referenceWork, RemoteEvent) {
	t.Helper()
	w := &referenceWork{}
	pool := sql.OpenDB(referenceConnector{w})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	for _, q := range []string{
		`CREATE TABLE projects(id INTEGER PRIMARY KEY,uid TEXT,name TEXT,deleted_at TEXT)`,
		`CREATE TABLE cron_jobs(project_id INTEGER,uid TEXT PRIMARY KEY,definition_event_uid TEXT)`,
		`CREATE TABLE cron_workflows(project_id INTEGER,uid TEXT PRIMARY KEY,definition_event_uid TEXT)`,
		`CREATE TABLE events(project_id INTEGER,uid TEXT PRIMARY KEY,type TEXT,origin_instance_uid TEXT,hlc_physical_ms INTEGER,hlc_counter INTEGER,payload TEXT)`,
		`CREATE TABLE cron_runs(id INTEGER PRIMARY KEY,uid TEXT UNIQUE,project_id INTEGER,job_uid TEXT,definition_event_uid TEXT,workflow_uid TEXT,workflow_definition_event_uid TEXT,occurrence_key TEXT,issue_uid TEXT,actor TEXT,teammate TEXT,executor_label TEXT,status TEXT,summary_json TEXT,revision INTEGER,created_at TEXT,started_at TEXT,ended_at TEXT,updated_at TEXT)`,
	} {
		_, err := pool.ExecContext(t.Context(), q)
		require.NoError(t, err)
	}
	job, version, workflow, workflowVersion, project := "01M40000000000000000000001", "01M40000000000000000000002", "01M40000000000000000000007", "01M40000000000000000000008", "01M40000000000000000000003"
	_, err := pool.ExecContext(t.Context(), `INSERT INTO projects VALUES(1,$1,'example-project',NULL)`, project)
	require.NoError(t, err)
	_, err = pool.ExecContext(t.Context(), `INSERT INTO cron_jobs VALUES(1,$1,$2)`, job, version)
	require.NoError(t, err)
	_, err = pool.ExecContext(t.Context(), `INSERT INTO cron_workflows VALUES(1,$1,$2)`, workflow, workflowVersion)
	require.NoError(t, err)
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	run := CronRun{UID: "01M40000000000000000000004", ProjectID: 1, JobUID: &job, DefinitionEventUID: &version, WorkflowUID: &workflow, WorkflowDefinitionEventUID: &workflowVersion, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}, Revision: 1, CreatedAt: at, UpdatedAt: at}
	body, err := json.Marshal(NewCronRunObservation(run, project))
	require.NoError(t, err)
	event := RemoteEvent{Type: "cron.run.observed", EventUID: "01M40000000000000000000005", ProjectUID: project, OriginInstanceUID: "01M40000000000000000000006", HLCPhysicalMS: at.UnixMilli(), Payload: body}
	// Unrelated accepted observations share another definition and project.
	otherJob, otherVersion := "01M50000000000000000000001", "01M50000000000000000000002"
	_, err = pool.ExecContext(t.Context(), `INSERT INTO cron_jobs VALUES(2,$1,$2)`, otherJob, otherVersion)
	require.NoError(t, err)
	for i := range history {
		previous := run
		previous.UID = fmt.Sprintf("01M5%022d", i+10)
		previous.JobUID = &otherJob
		previous.DefinitionEventUID = &otherVersion
		previous.WorkflowUID = nil
		previous.WorkflowDefinitionEventUID = nil
		raw, err := json.Marshal(NewCronRunObservation(previous, project))
		require.NoError(t, err)
		_, err = pool.ExecContext(t.Context(), `INSERT INTO events VALUES(2,$1,'cron.run.observed',$2,$3,0,$4)`, previous.UID, event.OriginInstanceUID, event.HLCPhysicalMS, string(raw))
		require.NoError(t, err)
		_, err = pool.ExecContext(t.Context(), `INSERT INTO cron_runs(project_id,uid,job_uid,definition_event_uid) VALUES(2,$1,$2,$3)`, previous.UID, otherJob, otherVersion)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	*w = referenceWork{}
	return tx, w, event
}

func TestCronReferenceWorkIgnoresUnrelatedHistory(t *testing.T) {
	for _, history := range []int{0, 256} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			tx, w, event := referenceWorkFixture(t, history)
			var in CronRunObservation
			require.NoError(t, json.Unmarshal(event.Payload, &in))
			sqlCron := CronSQL{Transact: func(_ context.Context, fn func(*sql.Tx) error) error { return fn(tx) }, WriteGate: func(context.Context, *sql.Tx, int64) error { return nil }, InsertEvent: func(context.Context, *sql.Tx, CronEvent) (Event, error) { return Event{UID: event.EventUID}, nil }}
			observed, err := sqlCron.ObserveRun(t.Context(), ObserveCronRun{ProjectID: 1, UID: in.UID, JobUID: in.JobUID, DefinitionEventUID: in.DefinitionEventUID, WorkflowUID: in.WorkflowUID, WorkflowDefinitionEventUID: in.WorkflowDefinitionEventUID, Actor: in.Actor, Status: in.Status, Summary: in.Summary})
			require.NoError(t, err)
			require.False(t, observed.Replayed)
			t.Logf("history=%d queries=%d rows=%d", history, w.queries, w.rows)
			require.LessOrEqual(t, w.queries, 12)
			require.LessOrEqual(t, w.rows, 8)
		})
	}
}
func TestCronReplayBatchReferenceWork(t *testing.T) {
	for _, size := range []int{1, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			tx, w, event := referenceWorkFixture(t, 256)
			var err error
			validator := NewCronReplayValidator(tx, false)
			for i := range size {
				var body CronRunObservation
				require.NoError(t, json.Unmarshal(event.Payload, &body))
				body.UID = fmt.Sprintf("01M6%022d", i+10)
				event.Payload, err = json.Marshal(body)
				require.NoError(t, err)
				event.EventUID = fmt.Sprintf("01M7%022d", i+10)
				require.NoError(t, validator.Validate(t.Context(), 1, event))
				_, err = tx.ExecContext(t.Context(), `INSERT INTO events VALUES(1,$1,$2,$3,$4,0,$5)`, event.EventUID, event.Type, event.OriginInstanceUID, event.HLCPhysicalMS, string(event.Payload))
				require.NoError(t, err)
			}
			t.Logf("batch=%d queries=%d rows=%d", size, w.queries, w.rows)
			require.LessOrEqual(t, w.queries, 12+size)
			require.LessOrEqual(t, w.rows, 8+size)
		})
	}
}

func TestCronReferenceEscapedHistoricalOwnership(t *testing.T) {
	tx, _, event := referenceWorkFixture(t, 1)
	otherJob, otherVersion := "01M50000000000000000000001", "01M50000000000000000000002"
	var payload string
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT payload FROM events WHERE project_id=2`).Scan(&payload))
	encode := func(s string) string {
		out := ""
		for _, c := range s {
			out += fmt.Sprintf("\\u%04x", c)
		}
		return out
	}
	payload = strings.ReplaceAll(strings.ReplaceAll(payload, otherJob, encode(otherJob)), otherVersion, encode(otherVersion))
	_, err := tx.ExecContext(t.Context(), `UPDATE events SET payload=$1 WHERE project_id=2`, payload)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM cron_jobs WHERE project_id=2`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM cron_runs WHERE project_id=2`)
	require.NoError(t, err)
	var body CronRunObservation
	require.NoError(t, json.Unmarshal(event.Payload, &body))
	body.JobUID = &otherJob
	body.DefinitionEventUID = &otherVersion
	body.WorkflowUID = nil
	body.WorkflowDefinitionEventUID = nil
	event.Payload, err = json.Marshal(body)
	require.NoError(t, err)
	require.ErrorIs(t, NewCronReplayValidator(tx, false).Validate(t.Context(), 1, event), ErrFederationIngestValidation)
}

func TestCronReferenceCompactedWrongKind(t *testing.T) {
	tx, _, event := referenceWorkFixture(t, 0)
	var body CronRunObservation
	require.NoError(t, json.Unmarshal(event.Payload, &body))
	body.DefinitionEventUID = body.WorkflowDefinitionEventUID
	body.WorkflowUID = nil
	body.WorkflowDefinitionEventUID = nil
	var err error
	event.Payload, err = json.Marshal(body)
	require.NoError(t, err)
	require.ErrorIs(t, NewCronReplayValidator(tx, false).Validate(t.Context(), 1, event), ErrFederationIngestValidation)
}

func TestCronReplayBatchRejectsLaterUnrelatedVersion(t *testing.T) {
	tx, _, event := referenceWorkFixture(t, 0)
	var body CronRunObservation
	require.NoError(t, json.Unmarshal(event.Payload, &body))
	job, version := "01M80000000000000000000001", "01M80000000000000000000002"
	body.JobUID = &job
	body.DefinitionEventUID = &version
	body.WorkflowUID = nil
	body.WorkflowDefinitionEventUID = nil
	var err error
	event.Payload, err = json.Marshal(body)
	require.NoError(t, err)
	validator := NewCronReplayValidator(tx, false)
	require.NoError(t, validator.Validate(t.Context(), 1, event), "unknown out-of-order version is retained")
	unrelated := event
	unrelated.EventUID = version
	unrelated.Type = "issue.updated"
	unrelated.Payload = []byte(`{}`)
	require.ErrorIs(t, validator.Validate(t.Context(), 1, unrelated), ErrFederationIngestValidation)
}

func TestCronReplayBatchProjectHistoryLoadedOnce(t *testing.T) {
	for _, size := range []int{1, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			tx, w, event := referenceWorkFixture(t, 256)
			for _, query := range []string{"UPDATE cron_jobs SET project_id=1 WHERE project_id=2", "UPDATE cron_runs SET project_id=1 WHERE project_id=2", "UPDATE events SET project_id=1 WHERE project_id=2"} {
				_, err := tx.ExecContext(t.Context(), query)
				require.NoError(t, err)
			}
			validator := NewCronReplayValidator(tx, false)
			for i := range size {
				var body CronRunObservation
				require.NoError(t, json.Unmarshal(event.Payload, &body))
				body.UID = fmt.Sprintf("01M6%022d", i+10)
				var err error
				event.Payload, err = json.Marshal(body)
				require.NoError(t, err)
				event.EventUID = fmt.Sprintf("01M7%022d", i+10)
				require.NoError(t, validator.Validate(t.Context(), 1, event))
			}
			t.Logf("project-history=256 batch=%d queries=%d rows=%d", size, w.queries, w.rows)
			require.LessOrEqual(t, w.queries, 12+size)
			require.LessOrEqual(t, w.rows, 264)
		})
	}
}
func TestCronMaterializeReferenceWork(t *testing.T) {
	for _, history := range []int{0, 256} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			tx, w, event := referenceWorkFixture(t, history)
			projection := FoldProjection{CronRuns: map[string]FoldCronRun{}}
			var body CronRunObservation
			require.NoError(t, json.Unmarshal(event.Payload, &body))
			for i := range 32 {
				run := body.Run()
				run.UID = fmt.Sprintf("01M6%022d", i+10)
				projection.CronRuns[run.UID] = FoldCronRun{CronRun: run, ProjectUID: event.ProjectUID}
			}
			require.NoError(t, materializeCronRuns(t.Context(), tx, 1, event.ProjectUID, projection, NewCronReplayValidator(tx, false)))
			t.Logf("history=%d materialize=32 queries=%d rows=%d", history, w.queries, w.rows)
			require.LessOrEqual(t, w.queries, 40)
			require.LessOrEqual(t, w.rows, 8)
		})
	}
}

func TestCronReplayDistinctColdPairsWork(t *testing.T) {
	for _, size := range []int{1, 32} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			tx, w, template := referenceWorkFixture(t, 256)
			events := make([]RemoteEvent, 0, size)
			for i := range size {
				var body CronRunObservation
				require.NoError(t, json.Unmarshal(template.Payload, &body))
				job, version := fmt.Sprintf("01M8%022d", i+10), fmt.Sprintf("01M9%022d", i+10)
				body.UID = fmt.Sprintf("01MB%022d", i+10)
				body.JobUID = &job
				body.DefinitionEventUID = &version
				body.WorkflowUID = nil
				body.WorkflowDefinitionEventUID = nil
				event := template
				var err error
				event.Payload, err = json.Marshal(body)
				require.NoError(t, err)
				event.EventUID = fmt.Sprintf("01MA%022d", i+10)
				events = append(events, event)
			}
			validator := NewCronReplayValidator(tx, false)
			require.NoError(t, validator.PrepareEvents(t.Context(), 1, events))
			for _, event := range events {
				require.NoError(t, validator.Validate(t.Context(), 1, event))
			}
			t.Logf("cold-pairs=%d unrelated-history=256 queries=%d rows=%d", size, w.queries, w.rows)
			require.LessOrEqual(t, w.queries, 3*size+10)
			require.LessOrEqual(t, w.rows, 8)
		})
	}
}

func TestCronReplayHistoricalDocumentFailureIsValidation(t *testing.T) {
	tx, _, event := referenceWorkFixture(t, 0)
	_, err := tx.ExecContext(t.Context(), `INSERT INTO events VALUES(1,'01MC0000000000000000000001','cron.run.observed',$1,$2,0,'{}')`, event.OriginInstanceUID, event.HLCPhysicalMS)
	require.NoError(t, err)
	require.ErrorIs(t, NewCronReplayValidator(tx, false).Validate(t.Context(), 1, event), ErrFederationIngestValidation)
}

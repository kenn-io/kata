package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Held rows are omitted from ordinary lists but remain fenced from reads and
// snapshots, including tombstones. Test-local SQL does not define production state.
func checkArgvCompatStoredReads(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Legacy", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	healthy, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Replacement", Actor: "worker", Definition: workflow.Definition})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	for _, document := range []string{`{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`, `{"version":1,"steps":null}`, `{"version":1,"steps":[null]}`, `{"version":1,"steps":[{"kind":null}]}`} {
		for _, deleted := range []any{nil, "2026-11-08T12:00:00Z"} {
			_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1,deleted_at=$2 WHERE uid=$3`, document, deleted, workflow.UID)
			require.NoError(t, err)
			_, err = store.CronWorkflow(ctx, project.ID, workflow.UID)
			require.Error(t, err)
			listed, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, healthy.UID, listed[0].UID)
			tx, err := q.BeginTx(ctx, nil)
			require.NoError(t, err)
			snapshot, snapshotErr := db.CronDefinitionSnapshots(ctx, tx, project)
			require.NoError(t, tx.Rollback())
			require.Error(t, snapshotErr)
			require.Empty(t, snapshot)
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1 WHERE uid=$2`, `{"version":1,"unexpected":true}`, workflow.UID)
	require.NoError(t, err)
	_, err = store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID, IncludeDeleted: true})
	require.Error(t, err, "unrelated decode errors must remain visible")
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1 WHERE uid=$2`, `{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`, workflow.UID)
	require.NoError(t, err)
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET deleted_at=NULL WHERE uid=$1`, workflow.UID)
	require.NoError(t, err)
	olderUID, err := uid.New()
	require.NoError(t, err)
	originUID, err := uid.New()
	require.NoError(t, err)
	definition, err := json.Marshal(workflow.Definition)
	require.NoError(t, err)
	payload, err := json.Marshal(db.CronDefinitionEvent{UID: workflow.UID, ProjectUID: project.UID, Name: workflow.Name, Definition: definition, Author: workflow.Author, CreatedAt: workflow.CreatedAt, UpdatedAt: workflow.UpdatedAt})
	require.NoError(t, err)
	projection := db.FoldEvents([]db.FoldEvent{{UID: olderUID, ProjectUID: project.UID, OriginInstanceUID: originUID, Type: "cron.workflow.created", HLCPhysicalMS: 1, Payload: payload}})
	tx, err := q.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, projection, db.NewCronReplayValidator(tx, backend.Name == "postgres")))
	require.NoError(t, tx.Commit())
	_, err = store.CronWorkflow(ctx, project.ID, workflow.UID)
	require.Error(t, err, "replay of older history must not replace a held projection")
	job, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job.Action.Prompt = ""
	job.Action.WorkflowUID = workflow.UID
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: job})
	require.ErrorIs(t, err, cron.ErrInvalid, "a new job must not use a held workflow")
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.ErrorIs(t, err, cron.ErrInvalid, "a new run must not cite a held revision")
	terminalUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: terminalUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err, "terminal evidence may cite the held revision")
	wrongUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: wrongUID, WorkflowUID: &healthy.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1}})
	require.Error(t, err, "terminal evidence still requires exact revision identity")
	// Model an owner restore that retains an unrepresentable event but cannot
	// store its workflow row. Snapshot and adoption must preserve that evidence.
	var original string
	require.NoError(t, q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1`, workflow.DefinitionEventUID).Scan(&original))
	var body db.CronDefinitionEvent
	require.NoError(t, json.Unmarshal([]byte(original), &body))
	_, err = q.ExecContext(ctx, `DELETE FROM cron_workflows WHERE uid=$1`, workflow.UID)
	require.NoError(t, err)
	for _, document := range []string{`null`, `{"version":2,"steps":[]}`, `{"version":1,"steps":[{"kind":"command","future":true}]}`} {
		body.Definition = jsontext.Value(document)
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, string(payload), workflow.DefinitionEventUID)
		require.NoError(t, err)
		tx, err := q.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = db.CronDefinitionSnapshots(ctx, tx, project)
		require.Error(t, err, "snapshot must refuse retained %s", document)
		_, err = db.PrepareCronAdoption(ctx, tx, project, project.UID, false)
		require.Error(t, err, "adoption must refuse retained %s", document)
		require.NoError(t, tx.Rollback())
		var retained string
		require.NoError(t, q.QueryRowContext(ctx, `SELECT payload FROM events WHERE uid=$1`, workflow.DefinitionEventUID).Scan(&retained))
		require.JSONEq(t, string(payload), retained)
	}
	return nil
}

func checkArgvCompatRetainedMaterialization(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	event := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"kind":"argv","argv":["true"]}]}`)
	fold := db.FoldEvents([]db.FoldEvent{{UID: event.EventUID, ProjectUID: project.UID, OriginInstanceUID: origin, Type: event.Type, HLCPhysicalMS: event.HLCPhysicalMS, Payload: event.Payload}})
	q := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	tx, err := q.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.Error(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, fold, db.NewCronReplayValidator(tx, backend.Name == "postgres")))
	require.NoError(t, tx.Rollback())
	tx, err = q.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, db.MaterializeCronDefinitions(ctx, tx, other.ID, other.UID, fold, db.NewCronReplayValidator(tx, backend.Name == "postgres")), "another project's history does not block this project")
	require.NoError(t, tx.Rollback())
	// A future field on an update or deletion cannot make the earlier
	// runnable definition eligible for materialization.
	for _, kind := range []string{"cron.workflow.updated", "cron.workflow.deleted"} {
		bad := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"kind":"command","future":true}]}`)
		if kind == "cron.workflow.deleted" {
			var body db.CronDefinitionEvent
			require.NoError(t, json.Unmarshal(bad.Payload, &body))
			deleted := body.UpdatedAt
			body.DeletedAt = &deleted
			bad.Payload, err = json.Marshal(body)
			require.NoError(t, err)
		}
		fold := db.FoldEvents([]db.FoldEvent{{UID: bad.EventUID, ProjectUID: project.UID, OriginInstanceUID: origin, Type: kind, HLCPhysicalMS: bad.HLCPhysicalMS, Payload: bad.Payload}})
		tx, err = q.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.ErrorIs(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, fold, db.NewCronReplayValidator(tx, backend.Name == "postgres")), db.ErrFederationIngestValidation)
		require.NoError(t, tx.Rollback())
	}
	for _, document := range []string{`null`, `{"steps":[]}`, `{"version":1,"steps":[{"kind":"command","future":true}]}`} {
		bad := argvCompatRemoteWorkflow(t, project, origin, document)
		projection := db.FoldEvents([]db.FoldEvent{{UID: bad.EventUID, ProjectUID: project.UID, OriginInstanceUID: origin, Type: bad.Type, HLCPhysicalMS: bad.HLCPhysicalMS, Payload: bad.Payload}})
		tx, err = q.BeginTx(ctx, nil)
		require.NoError(t, err)
		err = db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, projection, db.NewCronReplayValidator(tx, backend.Name == "postgres"))
		require.ErrorIs(t, err, db.ErrFederationIngestValidation, "unrepresentable historical document %s must fence replay before SQL", document)
		require.NoError(t, tx.Rollback())
	}
	return nil
}

func argvCompatRemoteWorkflow(t *testing.T, project db.Project, origin, definition string) db.RemoteEvent {
	t.Helper()
	workflowUID, err := uid.New()
	require.NoError(t, err)
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal(db.CronDefinitionEvent{UID: workflowUID, ProjectUID: project.UID, Name: "Example workflow", Definition: jsontext.Value(definition), Author: "worker", CreatedAt: at, UpdatedAt: at})
	require.NoError(t, err)
	return newRemoteEvent(t, project, nil, "cron.workflow.created", "worker", origin, at.UnixMilli(), payload)
}

// A reserved kind cannot enter through decode-only historical acceptance or
// preserve a valid prefix of a refused batch.
func checkArgvCompatRemoteRollback(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	good := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"key":"legacy","kind":"command","command":"true"}]}`)
	reserved := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`)
	binding, err := store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	binding.PullCursorEventID, binding.PushCursorEventID = 17, 23
	binding, err = store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	requireCursors := func() {
		current, err := store.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, binding.PullCursorEventID, current.PullCursorEventID)
		require.Equal(t, binding.PushCursorEventID, current.PushCursorEventID)
	}
	requireAbsent := func() {
		for _, eventUID := range []string{good.EventUID, reserved.EventUID} {
			_, err := store.EventsByUIDs(ctx, project.ID, []string{eventUID})
			require.ErrorIs(t, err, db.ErrNotFound)
		}
	}
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.InsertRemoteEvents(ctx, project.ID, []db.RemoteEvent{good, reserved})
	require.Error(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	requireAbsent()
	requireCursors()

	before, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	params := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: good}, {SourceEventID: 2, Event: reserved}}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.Error(t, err)
	after, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	requireAbsent()
	requireCursors()

	malformed := argvCompatRemoteWorkflow(t, project, origin, `{"version":1,"steps":[{"kind":null}]}`)
	params.Events = []db.FederationIngestEvent{{SourceEventID: 1, Event: malformed}}
	_, err = store.IngestFederationEvents(ctx, params)
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	_, err = store.EventsByUIDs(ctx, project.ID, []string{malformed.EventUID})
	require.ErrorIs(t, err, db.ErrNotFound)
	requireCursors()
	return nil
}

// Complete retained history and projections fence direct reads, independent of page cursors.
func checkArgvRetainedFeatureProbe(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "feature-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-feature-project")
	require.NoError(t, err)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Example", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "one", Kind: "command", Command: "echo argv"}}}})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		QueryRowContext(context.Context, string, ...any) *sql.Row
	})
	features, err := db.ProjectRequiredEventFeatures(ctx, q, project.UID)
	require.NoError(t, err)
	require.Equal(t, db.CronEventFeature, features)
	// Warm the native-job cache before introducing retained evidence.
	parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Parent", Author: "worker"})
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 7200}, Action: cron.Action{Kind: "notify", Recipient: "worker", Message: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "latest"}
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Native", Actor: "worker", Definition: job})
	require.NoError(t, err)
	_, err = db.ProjectRequiredEventFeatures(ctx, q, project.UID)
	require.NoError(t, err)
	legacy := `{"version":1,"steps":[{"kind":"command","command":"echo argv"}]}`
	supported := `{"version":1,"steps":[{"kind":"argv","argv":["true"]}]}`
	held := []string{`{"version":1,"steps":[{"kind":"argv"}]}`, `{"version":1,"steps":null}`, `{"version":1,"steps":[null]}`, `{"version":1,"steps":[{"kind":null}]}`}
	for _, deleted := range []any{nil, "2026-11-08T12:00:00Z"} {
		_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1,deleted_at=$2 WHERE uid=$3`, supported, deleted, workflow.UID)
		require.NoError(t, err)
		features, err = db.ProjectRequiredEventFeatures(ctx, q, project.UID)
		require.NoError(t, err)
		require.Equal(t, db.MergeEventFeatures(db.CronEventFeature, db.CronWorkflowArgvEventFeature), features)
		_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.SupportedEventFeatures + "," + db.CronWorkflowArgvEventFeature})
		require.NoError(t, err)
		for _, definition := range held {
			_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1 WHERE uid=$2`, definition, workflow.UID)
			require.NoError(t, err)
			_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.SupportedEventFeatures + "," + db.CronWorkflowArgvEventFeature})
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		}
	}
	_, err = q.ExecContext(ctx, `UPDATE cron_workflows SET definition_json=$1,deleted_at=NULL WHERE uid=$2`, legacy, workflow.UID)
	require.NoError(t, err)
	for _, kind := range []string{"cron.workflow.created", "cron.workflow.updated", "cron.workflow.deleted", "cron.workflow.restored", "cron.workflow.snapshot"} {
		event := argvCompatRemoteWorkflow(t, project, store.InstanceUID(), supported)
		_, err = q.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, event.EventUID, store.InstanceUID(), project.ID, project.Name, kind, "worker", string(event.Payload), event.HLCPhysicalMS, 0, event.ContentHash)
		require.NoError(t, err)
		features, err = db.ProjectRequiredEventFeatures(ctx, q, project.UID)
		require.NoError(t, err)
		require.Contains(t, features, db.CronWorkflowArgvEventFeature)
		require.ErrorIs(t, db.RequireEventFeatures(db.CronEventFeature+","+db.CronWorkflowArgvEventFeature+"_extra", features), db.ErrUnsupportedEventFeatures)
		_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: 1 << 60, Limit: 1, Features: db.SupportedEventFeatures})
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		_, err = q.ExecContext(ctx, `UPDATE events SET payload=$1 WHERE uid=$2`, `{"definition":null}`, event.EventUID)
		require.NoError(t, err)
		_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.SupportedEventFeatures + "," + db.CronWorkflowArgvEventFeature})
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		_, err = q.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, event.EventUID)
		require.NoError(t, err)
	}
	features, err = db.ProjectRequiredEventFeatures(ctx, q, other.UID)
	require.NoError(t, err)
	require.Empty(t, features)
	features, err = db.ProjectRequiredEventFeatures(ctx, q, project.UID)
	require.NoError(t, err)
	require.Equal(t, db.CronEventFeature, features)
	return nil
}

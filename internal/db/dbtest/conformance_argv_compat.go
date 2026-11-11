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
func checkArgvCompatStoredReads(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Legacy", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "first", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	healthy, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Replacement", Actor: "worker", Definition: workflow.Definition})
	require.NoError(t, err)
	q := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	for _, document := range []string{`{"version":1,"steps":[{"key":"reserved","kind":"argv"}]}`, `null`, `{"version":1,"steps":null}`, `{"version":1,"steps":[null]}`, `{"version":1,"steps":[{"kind":null}]}`} {
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
	require.NoError(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, projection, nil))
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
	return nil
}

func checkArgvCompatRetainedMaterialization(t *testing.T, store db.Storage) error {
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
	require.Error(t, db.MaterializeCronDefinitions(ctx, tx, project.ID, project.UID, fold, nil))
	require.NoError(t, tx.Rollback())
	tx, err = q.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, db.MaterializeCronDefinitions(ctx, tx, other.ID, other.UID, fold, nil), "another project's history does not block this project")
	require.NoError(t, tx.Rollback())
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
// preserve a valid prefix of a refused batch, even with a claimed capability.
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
	params := db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures + ",cron_workflow_argv_v1", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: good}, {SourceEventID: 2, Event: reserved}}}
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

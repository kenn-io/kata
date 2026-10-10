package dbtest

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func checkNativeCronDefinitions(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "example-cron")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-cron")
	require.NoError(t, err)
	workflowDef := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	workflow, event, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review", Definition: workflowDef, Actor: "worker"})
	require.NoError(t, err)
	require.Equal(t, event.UID, workflow.DefinitionEventUID)
	require.Equal(t, "cron.workflow.created", event.Type)
	require.Equal(t, int64(1), workflow.Revision)
	workflowRead, err := store.CronWorkflow(ctx, project.ID, workflow.UID)
	require.NoError(t, err)
	require.Equal(t, workflow, workflowRead)
	workflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Equal(t, []db.CronWorkflow{workflow}, workflows)
	_, err = store.CronWorkflow(ctx, other.ID, workflow.UID)
	require.ErrorIs(t, err, db.ErrNotFound)

	jobDef, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	jobDef.Action.Prompt = ""
	jobDef.Action.WorkflowUID = workflow.UID
	t.Run("dependency diagnostics", func(t *testing.T) {
		missing := jobDef
		missing.Action.WorkflowUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
		_, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Missing", Definition: missing, Actor: "worker"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "missing workflow dependency")
		deleted, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "worker", Deleted: true})
		require.NoError(t, err)
		_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Deleted", Definition: jobDef, Actor: "worker"})
		require.ErrorIs(t, err, cron.ErrInvalid)
		require.ErrorContains(t, err, "tombstoned workflow dependency")
		workflow, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: deleted.DefinitionEventUID, Actor: "worker"})
		require.NoError(t, err)
	})
	_, _, err = store.PutCronJob(ctx, db.PutCronJob{ProjectID: other.ID, Name: "Wrong project", Definition: jobDef, Actor: "worker"})
	require.Error(t, err)
	job, events, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Definition: jobDef, Actor: "worker"})
	require.NoError(t, err)
	require.Equal(t, events[0].UID, job.DefinitionEventUID)
	require.Equal(t, events[0].HLCPhysicalMS, job.DefinitionHLC.PhysicalMS)
	require.Equal(t, "worker", events[0].Actor)
	require.Equal(t, int64(1), job.Revision)
	read, err := store.CronJob(ctx, project.ID, job.UID)
	require.NoError(t, err)
	require.Equal(t, job, read)
	_, err = store.CronJob(ctx, other.ID, job.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	update := db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: "Edited", Definition: jobDef, ExpectedEventUID: job.DefinitionEventUID, Actor: "editor"}
	updated, events, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.Greater(t, events[0].ID, before)
	require.Equal(t, int64(2), updated.Revision)
	require.NotEqual(t, job.DefinitionEventUID, updated.DefinitionEventUID)
	require.Equal(t, job.Author, updated.Author)
	_, _, err = store.PutCronJob(ctx, update)
	require.ErrorIs(t, err, db.ErrCronConflict)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, events[0].ID, after, "conflicting write emits no event")
	update.ExpectedEventUID = updated.DefinitionEventUID
	update.Deleted = true
	deleted, deletedEvent, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.NotNil(t, deleted.DeletedAt)
	require.Equal(t, "cron.job.deleted", deletedEvent[0].Type)
	jobs, err = store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Empty(t, jobs)
	update.ExpectedEventUID = deleted.DefinitionEventUID
	update.Deleted = false
	restored, events, err := store.PutCronJob(ctx, update)
	require.NoError(t, err)
	require.Nil(t, restored.DeletedAt)
	require.Equal(t, "cron.job.restored", events[0].Type)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
	require.Equal(t, job.UID, payload["uid"])
	deleteWorkflow := db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "worker", Deleted: true}
	_, _, err = store.PutCronWorkflow(ctx, deleteWorkflow)
	require.ErrorIs(t, err, cron.ErrInvalid, "a live job must not lose its workflow")
	require.ErrorContains(t, err, "used by live job "+job.UID)
	kept, err := store.CronWorkflow(ctx, project.ID, workflow.UID)
	require.NoError(t, err)
	require.Nil(t, kept.DeletedAt, "a refused delete leaves the workflow live")
	retired, _, err := store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: restored.Name, Definition: jobDef, ExpectedEventUID: restored.DefinitionEventUID, Actor: "editor", Deleted: true})
	require.NoError(t, err)
	_, _, err = store.PutCronWorkflow(ctx, deleteWorkflow)
	require.NoError(t, err, "a workflow used only by deleted jobs can be deleted")
	restored = retired
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "worker"})
	require.NoError(t, err)
	update.ExpectedEventUID = restored.DefinitionEventUID
	_, _, err = store.PutCronJob(ctx, update)
	require.Error(t, err)
	_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Archived", Definition: workflowDef, Actor: "worker"})
	require.Error(t, err)
	return nil
}

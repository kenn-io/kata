package dbtest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A backup taken with deleted rows restores tombstoned definitions as
// tombstones and run evidence field for field.
func checkCronBackupTombstoneRoundTrip(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, source, project)
	run := observeCompleteRun(t, source, project, job, workflow)
	deletedJob, _, err := source.PutCronJob(ctx, db.PutCronJob{
		UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition,
		ExpectedEventUID: job.DefinitionEventUID, Actor: "worker", Deleted: true,
	})
	require.NoError(t, err)
	deletedWorkflow, _, err := source.PutCronWorkflow(ctx, db.PutCronWorkflow{
		UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition,
		ExpectedEventUID: workflow.DefinitionEventUID, Actor: "worker", Deleted: true,
	})
	require.NoError(t, err)
	require.NotNil(t, deletedJob.DeletedAt)
	require.NotNil(t, deletedWorkflow.DeletedAt)

	records, err := CollectImportRecords(ctx, source, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
	target, err := restored.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)

	restoredJob, err := restored.CronJob(ctx, target.ID, job.UID)
	require.NoError(t, err)
	require.NotNil(t, restoredJob.DeletedAt, "a deleted job stays deleted after restore")
	require.True(t, deletedJob.DeletedAt.Equal(*restoredJob.DeletedAt))
	require.Equal(t, deletedJob.DefinitionEventUID, restoredJob.DefinitionEventUID)
	restoredWorkflow, err := restored.CronWorkflow(ctx, target.ID, workflow.UID)
	require.NoError(t, err)
	require.NotNil(t, restoredWorkflow.DeletedAt, "a deleted workflow stays deleted after restore")
	require.True(t, deletedWorkflow.DeletedAt.Equal(*restoredWorkflow.DeletedAt))
	require.Equal(t, deletedWorkflow.DefinitionEventUID, restoredWorkflow.DefinitionEventUID)

	liveJobs, err := restored.ListCronJobs(ctx, db.CronList{ProjectID: target.ID})
	require.NoError(t, err)
	require.Empty(t, liveJobs)
	allJobs, err := restored.ListCronJobs(ctx, db.CronList{ProjectID: target.ID, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, allJobs, 1)
	require.Equal(t, job.UID, allJobs[0].UID)
	liveWorkflows, err := restored.ListCronWorkflows(ctx, db.CronList{ProjectID: target.ID})
	require.NoError(t, err)
	require.Empty(t, liveWorkflows)
	allWorkflows, err := restored.ListCronWorkflows(ctx, db.CronList{ProjectID: target.ID, IncludeDeleted: true})
	require.NoError(t, err)
	require.Len(t, allWorkflows, 1)
	require.Equal(t, workflow.UID, allWorkflows[0].UID)

	restoredRun, err := restored.CronRun(ctx, target.ID, run.UID)
	require.NoError(t, err)
	run.ID, run.ProjectID = restoredRun.ID, restoredRun.ProjectID
	require.Equal(t, run, restoredRun, "run evidence restores exactly")
	return nil
}

// The explicit project purge removes the project's cron definitions and run
// history and leaves another project's cron data in place.
func checkCronProjectPurge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	purged, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	kept, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	purgedJob, purgedWorkflow := nativeFederationDefinitions(t, store, purged)
	purgedRun := observeCompleteRun(t, store, purged, purgedJob, purgedWorkflow)
	keptJob, keptWorkflow := nativeFederationDefinitions(t, store, kept)
	keptRun := observeCompleteRun(t, store, kept, keptJob, keptWorkflow)

	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: purged.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: purged.ID, Actor: "worker"})
	require.NoError(t, err, "purging a project with cron data succeeds")

	_, err = store.ProjectByID(ctx, purged.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.CronJob(ctx, purged.ID, purgedJob.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.CronWorkflow(ctx, purged.ID, purgedWorkflow.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.CronRun(ctx, purged.ID, purgedRun.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
	jobs, err := collectExport(store.ExportCronJobs(ctx, db.ExportFilter{IncludeDeleted: true}))
	require.NoError(t, err)
	require.Len(t, jobs, 1, "only the kept project's job remains")
	require.Equal(t, keptJob.UID, jobs[0].UID)
	workflows, err := collectExport(store.ExportCronWorkflows(ctx, db.ExportFilter{IncludeDeleted: true}))
	require.NoError(t, err)
	require.Len(t, workflows, 1, "only the kept project's workflow remains")
	require.Equal(t, keptWorkflow.UID, workflows[0].UID)
	runs, err := collectExport(store.ExportCronRuns(ctx, db.ExportFilter{IncludeDeleted: true}))
	require.NoError(t, err)
	require.Len(t, runs, 1, "only the kept project's run remains")
	require.Equal(t, keptRun.UID, runs[0].UID)

	gotJob, err := store.CronJob(ctx, kept.ID, keptJob.UID)
	require.NoError(t, err)
	require.Equal(t, keptJob, gotJob)
	gotWorkflow, err := store.CronWorkflow(ctx, kept.ID, keptWorkflow.UID)
	require.NoError(t, err)
	require.Equal(t, keptWorkflow, gotWorkflow)
	gotRun, err := store.CronRun(ctx, kept.ID, keptRun.UID)
	require.NoError(t, err)
	require.Equal(t, keptRun, gotRun)
	return nil
}

func observeCompleteRun(t *testing.T, store db.Storage, project db.Project, job db.CronJob, workflow db.CronWorkflow) db.CronRun {
	t.Helper()
	runUID, err := uid.New()
	require.NoError(t, err)
	started := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	ended := started.Add(90 * time.Second)
	teammate, executor, occurrence := "reviewer", "example-executor", "manual:review"
	observed, err := store.ObserveCronRun(t.Context(), db.ObserveCronRun{
		ProjectID: project.ID, UID: runUID, JobUID: &job.UID,
		DefinitionEventUID: &job.DefinitionEventUID, WorkflowUID: &workflow.UID,
		WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, OccurrenceKey: &occurrence,
		Actor: "worker", Teammate: &teammate, ExecutorLabel: &executor, Status: "succeeded",
		Summary:   cron.Summary{Version: 1, Message: "Reviewed", InputTokens: 12, OutputTokens: 34},
		StartedAt: &started, EndedAt: &ended,
	})
	require.NoError(t, err)
	return observed.Run
}

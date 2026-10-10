package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCronDefinitionExportDeletedFilter(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	jobDefinition, err := cron.ParseJob([]byte(`{"version":1,"kind":"job","trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"Review"},"issue":{"kind":"per-run","title":"Review"},"overlap":"forbid","catchup":"skip"}`))
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: jobDefinition})
	require.NoError(t, err)
	workflowDefinition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "review", Kind: "command", Command: "git status"}}}
	workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: "worker", Definition: workflowDefinition})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{
		ProjectID:          project.ID,
		UID:                runUID,
		JobUID:             &job.UID,
		DefinitionEventUID: &job.DefinitionEventUID,
		Actor:              "worker",
		Status:             "succeeded",
		Summary:            cron.Summary{Version: 1},
	})
	require.NoError(t, err)
	deletedJob, _, err := store.PutCronJob(ctx, db.PutCronJob{
		UID:              job.UID,
		ProjectID:        project.ID,
		Name:             job.Name,
		Definition:       job.Definition,
		ExpectedEventUID: job.DefinitionEventUID,
		Actor:            "worker",
		Deleted:          true,
	})
	require.NoError(t, err)
	deletedWorkflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{
		UID:              workflow.UID,
		ProjectID:        project.ID,
		Name:             workflow.Name,
		Definition:       workflow.Definition,
		ExpectedEventUID: workflow.DefinitionEventUID,
		Actor:            "worker",
		Deleted:          true,
	})
	require.NoError(t, err)

	liveJobs := []db.CronJobExport{}
	for value, err := range store.ExportCronJobs(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		liveJobs = append(liveJobs, value)
	}
	require.Empty(t, liveJobs, "default exports exclude tombstoned jobs")
	allJobs := []db.CronJobExport{}
	for value, err := range store.ExportCronJobs(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		require.NoError(t, err)
		allJobs = append(allJobs, value)
	}
	require.Len(t, allJobs, 1)
	require.Equal(t, job.UID, allJobs[0].UID)
	require.Equal(t, deletedJob.DeletedAt, allJobs[0].DeletedAt)

	liveWorkflows := []db.CronWorkflowExport{}
	for value, err := range store.ExportCronWorkflows(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		liveWorkflows = append(liveWorkflows, value)
	}
	require.Empty(t, liveWorkflows, "default exports exclude tombstoned workflows")
	allWorkflows := []db.CronWorkflowExport{}
	for value, err := range store.ExportCronWorkflows(ctx, db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}) {
		require.NoError(t, err)
		allWorkflows = append(allWorkflows, value)
	}
	require.Len(t, allWorkflows, 1)
	require.Equal(t, workflow.UID, allWorkflows[0].UID)
	require.Equal(t, deletedWorkflow.DeletedAt, allWorkflows[0].DeletedAt)

	liveRuns := []db.CronRunExport{}
	for value, err := range store.ExportCronRuns(ctx, db.ExportFilter{ProjectID: &project.ID}) {
		require.NoError(t, err)
		liveRuns = append(liveRuns, value)
	}
	require.Len(t, liveRuns, 1, "run history remains exportable after its definition is tombstoned")
	require.Equal(t, runUID, liveRuns[0].UID)
	return nil
}

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// RunNativeCronTypedIdentity characterizes valid native state through ordinary
// writes and reads. Job and workflow UIDs belong to distinct typed namespaces;
// no scoped projection, raw SQL or injected row establishes these fixtures.
func RunNativeCronTypedIdentity(t *testing.T, store db.Storage) {
	t.Helper()
	ctx := t.Context()
	workflowDefinition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	jobDefinition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Inspect example work"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example work"}, Overlap: "forbid", Catchup: "skip"}

	t.Run("same_project_same_uid", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "same-project")
		require.NoError(t, err)
		workflow, workflowEvent, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "visible-workflow-marker", Definition: workflowDefinition, Actor: "worker"})
		require.NoError(t, err)
		// The ordinary manual/execute pattern needs no workflow dependency. Reuse
		// the workflow's UID as the job identity without bypassing creation policy.
		job, jobEvents, err := store.PutCronJob(ctx, db.PutCronJob{UID: workflow.UID, ProjectID: project.ID, Name: "same-project-job-marker", Definition: jobDefinition, Actor: "worker"})
		require.NoError(t, err)
		require.Equal(t, workflow.UID, job.UID)
		require.Equal(t, "cron.workflow.created", workflowEvent.Type)
		require.NotEmpty(t, jobEvents)
		require.Equal(t, "cron.job.created", jobEvents[0].Type)
		require.NotEqual(t, workflow.DefinitionEventUID, job.DefinitionEventUID, "typed definitions have independent source winners")
		gotWorkflow, err := store.CronWorkflow(ctx, project.ID, workflow.UID)
		require.NoError(t, err)
		gotJob, err := store.CronJob(ctx, project.ID, job.UID)
		require.NoError(t, err)
		require.Equal(t, workflow, gotWorkflow)
		require.Equal(t, job, gotJob)
		workflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		require.Equal(t, []db.CronWorkflow{workflow}, workflows)
		jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: project.ID})
		require.NoError(t, err)
		require.Equal(t, []db.CronJob{job}, jobs)
		t.Logf("native coexistence: project=%s workflow_uid=%s job_uid=%s distinct creation events; both typed reads/lists retain their markers", project.Name, workflow.UID, job.UID)
	})

	t.Run("workflow_a_job_b_same_uid", func(t *testing.T) {
		visible, err := store.CreateProject(ctx, "visible-project")
		require.NoError(t, err)
		hidden, err := store.CreateProject(ctx, "hidden-project")
		require.NoError(t, err)
		workflow, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: visible.ID, Name: "visible-workflow-marker", Definition: workflowDefinition, Actor: "worker"})
		require.NoError(t, err)
		dependency, _, err := store.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: hidden.ID, Name: "hidden-workflow-dependency-marker", Definition: workflowDefinition, Actor: "worker"})
		require.NoError(t, err)
		// Keep the action dependency local to the job's project. Its UID deliberately
		// matches the unrelated visible workflow, not its own workflow dependency.
		definition := jobDefinition
		definition.Action.Prompt = ""
		definition.Action.WorkflowUID = dependency.UID
		const hiddenJobMarker = "hidden-job-marker"
		job, events, err := store.PutCronJob(ctx, db.PutCronJob{UID: workflow.UID, ProjectID: hidden.ID, Name: hiddenJobMarker, Definition: definition, Actor: "worker"})
		require.NoError(t, err)
		require.Equal(t, workflow.UID, job.UID)
		require.NotEqual(t, workflow.ProjectID, job.ProjectID)
		require.NotEqual(t, dependency.UID, job.UID)
		require.NotEqual(t, workflow.DefinitionEventUID, job.DefinitionEventUID)
		require.NotEmpty(t, events)
		require.Equal(t, "cron.job.created", events[0].Type)
		gotWorkflow, err := store.CronWorkflow(ctx, visible.ID, workflow.UID)
		require.NoError(t, err)
		require.Equal(t, workflow, gotWorkflow)
		gotJob, err := store.CronJob(ctx, hidden.ID, job.UID)
		require.NoError(t, err)
		require.Equal(t, job, gotJob)
		require.Equal(t, hiddenJobMarker, gotJob.Name)
		gotDependency, err := store.CronWorkflow(ctx, hidden.ID, dependency.UID)
		require.NoError(t, err)
		require.Equal(t, dependency, gotDependency)
		_, err = store.CronJob(ctx, visible.ID, job.UID)
		require.ErrorIs(t, err, db.ErrNotFound, "job reads retain their typed project ownership")
		_, err = store.CronWorkflow(ctx, hidden.ID, workflow.UID)
		require.ErrorIs(t, err, db.ErrNotFound, "workflow reads do not resolve the colliding hidden job")
		jobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: hidden.ID})
		require.NoError(t, err)
		require.Equal(t, []db.CronJob{job}, jobs)
		visibleJobs, err := store.ListCronJobs(ctx, db.CronList{ProjectID: visible.ID})
		require.NoError(t, err)
		require.Empty(t, visibleJobs)
		workflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: visible.ID})
		require.NoError(t, err)
		require.Equal(t, []db.CronWorkflow{workflow}, workflows)
		hiddenWorkflows, err := store.ListCronWorkflows(ctx, db.CronList{ProjectID: hidden.ID})
		require.NoError(t, err)
		require.Equal(t, []db.CronWorkflow{dependency}, hiddenWorkflows)
		t.Logf("native coexistence: workflow_project=%s job_project=%s shared_uid=%s hidden_job_marker=%s dependency remains local", visible.Name, hidden.Name, workflow.UID, hiddenJobMarker)
	})
}

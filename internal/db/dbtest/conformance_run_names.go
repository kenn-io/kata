package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Run reads name their job, workflow and issue so a client needs no second
// read to label a page. An issue deleted after the run still gives its short
// ID, a soft-deleted job or workflow still names its runs, and a run without a
// workflow or issue leaves those names unset.
func checkRunNames(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, workflow := nativeFederationDefinitions(t, store, project)
	live, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Live", Author: "worker"})
	require.NoError(t, err)
	gone, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Gone", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, gone.ID, "worker")
	require.NoError(t, err)

	observe := func(withWorkflow bool, issue db.Issue) string {
		runUID, err := uid.New()
		require.NoError(t, err)
		in := db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, IssueUID: &issue.UID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}}
		if withWorkflow {
			in.WorkflowUID, in.WorkflowDefinitionEventUID = &workflow.UID, &workflow.DefinitionEventUID
		}
		_, err = store.ObserveCronRun(ctx, in)
		require.NoError(t, err)
		return runUID
	}
	full := observe(true, live)
	bare := observe(false, gone)
	runUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)

	type names struct{ job, workflow, issue string }
	of := func(r db.CronRun) names {
		v := func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		}
		return names{v(r.JobName), v(r.WorkflowName), v(r.IssueShortID)}
	}
	want := map[string]names{
		full:   {job.Name, workflow.Name, live.ShortID},
		bare:   {job.Name, "", gone.ShortID},
		runUID: {"", workflow.Name, ""},
	}
	page, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, page, len(want))
	for _, run := range page {
		require.Equal(t, want[run.UID], of(run), "list row %s", run.UID)
	}
	for id, expected := range want {
		run, err := store.CronRun(ctx, project.ID, id)
		require.NoError(t, err)
		require.Equal(t, expected, of(run), "show row %s", id)
	}
	byJob, err := store.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID, JobUID: job.UID})
	require.NoError(t, err)
	require.Len(t, byJob, 2)

	_, _, err = store.PutCronJob(ctx, db.PutCronJob{UID: job.UID, ProjectID: project.ID, Name: job.Name, Definition: job.Definition, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker", Deleted: true})
	require.NoError(t, err)
	_, _, err = store.PutCronWorkflow(ctx, db.PutCronWorkflow{UID: workflow.UID, ProjectID: project.ID, Name: workflow.Name, Definition: workflow.Definition, ExpectedEventUID: workflow.DefinitionEventUID, Actor: "worker", Deleted: true})
	require.NoError(t, err)
	for id, expected := range want {
		run, err := store.CronRun(ctx, project.ID, id)
		require.NoError(t, err)
		require.Equal(t, expected, of(run), "show row after deleting definitions %s", id)
	}
	return nil
}

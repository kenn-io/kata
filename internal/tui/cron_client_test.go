package tui

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronClientReadsStoredRecords(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	project, err := c.EnsureProject(ctx, "cron-example", "worker")
	require.NoError(t, err)
	client, err := c.generatedClient()
	require.NoError(t, err)
	workflow, err := client.CreateCronWorkflowWithResponse(ctx, &generated.CreateCronWorkflowRequestOptions{
		PathParams: &generated.CreateCronWorkflowPath{ProjectID: project.ID},
		Body: &generated.CreateCronWorkflowBody{
			Actor: new("worker"), Name: "Summary flow",
			Definition: generated.CronWorkflowDefinition{
				Version: 1,
				Steps: []generated.CronWorkflowStep{{
					Key: "summarize", Kind: "prompt", Prompt: new("Summarize"),
				}},
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 201, workflow.HTTPResponse.StatusCode, string(workflow.Body))
	var w struct {
		Workflow api.CronWorkflow `json:"workflow"`
	}
	require.NoError(t, json.Unmarshal(workflow.Body, &w))
	job, err := client.CreateCronJobWithResponse(ctx, &generated.CreateCronJobRequestOptions{
		PathParams: &generated.CreateCronJobPath{ProjectID: project.ID},
		Body: &generated.CreateCronJobBody{
			Actor: new("worker"), Name: "Daily summary",
			Definition: generated.CronJobDefinition{
				Version: 1, Kind: "job", Enabled: true,
				Trigger: generated.Trigger{Kind: "manual"},
				Action:  generated.Action{Kind: "execute", WorkflowUID: &w.Workflow.UID},
				Issue:   &generated.IssuePolicy{Kind: "per-run", Title: new("Summary result")},
				Overlap: "forbid", Catchup: "skip",
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 201, job.HTTPResponse.StatusCode, string(job.Body))
	var j struct {
		Job api.CronJob `json:"job"`
	}
	require.NoError(t, json.Unmarshal(job.Body, &j))
	run, err := client.ObserveCronRunWithResponse(ctx, &generated.ObserveCronRunRequestOptions{
		PathParams: &generated.ObserveCronRunPath{ProjectID: project.ID, RunUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		Body: &generated.ObserveCronRunBody{
			Actor: new("worker"), Status: "succeeded",
			JobUID: &j.Job.UID, DefinitionEventUID: &j.Job.DefinitionEventUID,
			WorkflowUID: &w.Workflow.UID, WorkflowDefinitionEventUID: &w.Workflow.DefinitionEventUID,
			Summary: generated.Summary{Version: 1, Message: new("Report ready")},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 200, run.HTTPResponse.StatusCode, string(run.Body))
	jobs, err := c.ListCronJobs(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Equal(t, "Daily summary", jobs[0].Name)
	require.Equal(t, project.ID, jobs[0].ProjectID)
	workflows, err := c.ListCronWorkflows(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, workflows, 1)
	require.Equal(t, "Summary flow", workflows[0].Name)
	runs, err := c.ListCronRuns(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "Report ready", deref(runs[0].Summary.Message))
	require.Equal(t, project.ID, runs[0].ProjectID)
}

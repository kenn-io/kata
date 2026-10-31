package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
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

func cronDaemonFixture(t *testing.T) (*Client, int64) {
	t.Helper()
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-write-example", "alice")
	require.NoError(t, err)
	return c, project.ID
}

func manualExecuteJob(options string) cron.JobDefinition {
	return cron.JobDefinition{
		Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Summarize"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Summary"},
		Overlap: "forbid", Catchup: "latest",
		Options: jsontext.Value(options),
	}
}

func TestCronClientWriteKeepsOptionsExactAndGuardsEdits(t *testing.T) {
	c, project := cronDaemonFixture(t)
	ctx := t.Context()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	const options = `{"future":{"keep":[1,2]},"big":12345678901234567890123,"exact":1.10}`

	created, err := c.PutCronJob(ctx, project, uid, CronJobWrite{Name: "Digest", Actor: "alice", Definition: manualExecuteJob(options)})
	require.NoError(t, err)
	shown, err := c.ShowCronJob(ctx, project, uid)
	require.NoError(t, err)
	require.Equal(t, created.DefinitionEventUID, shown.DefinitionEventUID)
	require.JSONEq(t, options, string(shown.Definition.Options))
	require.Contains(t, string(shown.Definition.Options), "12345678901234567890123")
	require.Contains(t, string(shown.Definition.Options), "1.10")

	shown.Definition.Enabled = false
	_, err = c.PutCronJob(ctx, project, uid, CronJobWrite{Name: "Digest", Actor: "alice", Definition: shown.Definition, ExpectedEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB9"})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 409, apiErr.Status)

	updated, err := c.PutCronJob(ctx, project, uid, CronJobWrite{Name: "Digest", Actor: "alice", Definition: shown.Definition, ExpectedEventUID: shown.DefinitionEventUID})
	require.NoError(t, err)
	require.False(t, updated.Definition.Enabled)
	require.Contains(t, string(updated.Definition.Options), "12345678901234567890123")

	require.NoError(t, c.ArchiveCronJob(ctx, project, uid, updated.DefinitionEventUID, "alice"))
	live, err := c.ListCronJobs(ctx, project)
	require.NoError(t, err)
	require.Empty(t, live)
	archived, err := c.ListArchivedCronJobs(ctx, project)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.NoError(t, c.RestoreCronJob(ctx, project, uid, archived[0].DefinitionEventUID, "alice"))
	live, err = c.ListCronJobs(ctx, project)
	require.NoError(t, err)
	require.Len(t, live, 1)
}

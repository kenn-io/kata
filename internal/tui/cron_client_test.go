package tui

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
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
		Workflow kataapi.CronWorkflow `json:"workflow"`
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
		Job kataapi.CronJob `json:"job"`
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

func manualExecuteJob(options string) cron.JobDefinition {
	return cron.JobDefinition{
		Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Line one\nLine two"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Digest", ScheduledOffsetSeconds: new(int64(3600))},
		Overlap: "forbid", Catchup: "latest",
		Options: jsontext.Value(options),
	}
}

func TestCronClientWriteGuardsEdits(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-write-example", "alice")
	require.NoError(t, err)
	ctx := t.Context()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, kataapi.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: kataapi.CronJobDefinitionFrom(manualExecuteJob(`{}`))}))
	shown, err := c.ShowCronJob(ctx, project.ID, uid)
	require.NoError(t, err)

	shown.Definition.Enabled = false
	err = c.PutCronJob(ctx, project.ID, uid, kataapi.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: kataapi.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB9"})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 409, apiErr.Status)

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, kataapi.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: kataapi.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: shown.DefinitionEventUID}))
	updated, err := c.ShowCronJob(ctx, project.ID, uid)
	require.NoError(t, err)
	require.False(t, updated.Definition.Enabled)
}

func TestCronJobRecordRefusesDefinitionsItWouldDrop(t *testing.T) {
	body := `{"job":{"uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","name":"Digest","definition_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FB2","definition":{"version":1,"kind":"job","enabled":true,"trigger":{"kind":"manual"},"action":{"kind":"execute","prompt":"x"},"issue":{"kind":"per-run","title":"t"},"overlap":"forbid","catchup":"latest","options":{"free":1},"newer_member":{"a":1}}}}`
	_, err := decodeCronJobRecord([]byte(body))
	require.ErrorContains(t, err, "newer_member")
	require.ErrorContains(t, err, "newer kata")
}

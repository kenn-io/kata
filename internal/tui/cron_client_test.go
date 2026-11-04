package tui

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronClientEnabledTimeoutDoesNotChangeReadTimeout(t *testing.T) {
	var remaining []time.Duration
	hc := &http.Client{Timeout: defaultHTTPTimeout, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		require.True(t, ok)
		remaining = append(remaining, time.Until(deadline))
		return nil, errors.New("request stopped after capturing its deadline")
	})}
	c := NewClient("https://daemon.example", hc)
	_, err := c.CronJobEnabled(t.Context(), 42, "example-job", "example-event", "operator", false)
	require.Error(t, err)
	_, err = c.ListCronJobs(t.Context(), 42)
	require.Error(t, err)

	require.Len(t, remaining, 2)
	require.Greater(t, remaining[0], 15*time.Second, "enabled actions must outlive the daemon's 15-second federation forward")
	require.Greater(t, remaining[1], 4*time.Second)
	require.LessOrEqual(t, remaining[1], defaultHTTPTimeout, "ordinary reads keep the default timeout")
	require.Equal(t, defaultHTTPTimeout, hc.Timeout, "the shared client keeps its default timeout")
}

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

func TestCronClientWriteGuardsEdits(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := c.EnsureProject(t.Context(), "cron-write-example", "alice")
	require.NoError(t, err)
	ctx := t.Context()
	const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(manualExecuteJob(`{}`))}))
	shown, err := c.ShowCronJob(ctx, project.ID, uid)
	require.NoError(t, err)

	shown.Definition.Enabled = false
	err = c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: "01ARZ3NDEKTSV4RRFFQ69G5FB9"})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "cron_conflict", apiErr.Code)
	require.Equal(t, 409, apiErr.Status)

	require.NoError(t, c.PutCronJob(ctx, project.ID, uid, api.PutCronJobBody{Name: "Digest", Actor: "alice", Definition: api.CronJobDefinitionFrom(shown.Definition), ExpectedEventUID: shown.DefinitionEventUID}))
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

func TestCronClientEnabledActionsKeepRevisionAndOptions(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	definition := executeRecord(`{"large":9007199254740993}`).Definition
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	paused, err := c.CronJobEnabled(t.Context(), project.ID, job.UID, job.DefinitionEventUID, "operator", false)
	require.NoError(t, err)
	require.False(t, paused.Definition.Enabled)
	require.Equal(t, job.Definition.Options, paused.Definition.Options)
	_, err = c.CronJobEnabled(t.Context(), project.ID, job.UID, job.DefinitionEventUID, "operator", true)
	require.Error(t, err)
	resumed, err := c.CronJobEnabled(t.Context(), project.ID, job.UID, paused.DefinitionEventUID, "operator", true)
	require.NoError(t, err)
	require.Equal(t, definition, resumed.Definition)
}

func TestCronClientRuntimeRequestsRoundTrip(t *testing.T) {
	env := testenv.New(t)
	c := NewClient(env.URL, env.HTTP)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "request-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Summary", Actor: "operator", Definition: executeRecord(`{}`).Definition})
	require.NoError(t, err)
	id, err := uid.New()
	require.NoError(t, err)
	first, err := c.RequestCron(ctx, project.ID, id, db.CronRequestKindRun, job.UID, "operator")
	require.NoError(t, err)
	require.Equal(t, id, first.Request.UID)
	require.Equal(t, job.DefinitionEventUID, first.Request.DefinitionEventUID)
	require.NotNil(t, first.Event)
	replay, err := c.RequestCron(ctx, project.ID, id, db.CronRequestKindRun, job.UID, "operator")
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, first.Request, replay.Request)
	require.Nil(t, replay.Event)
	requests, err := c.ListCronRequests(ctx, project.ID)
	require.NoError(t, err)
	require.Len(t, requests, 1)
	require.Equal(t, id, requests[0].UID)
	require.Equal(t, "pending", requests[0].State)
	runID, err := uid.New()
	require.NoError(t, err)
	observed, err := env.DB.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runID, Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	shown, err := c.ShowCronRun(ctx, project.ID, runID)
	require.NoError(t, err)
	require.Equal(t, observed.Run, shown)
	cancelID, err := uid.New()
	require.NoError(t, err)
	canceled, err := c.RequestCron(ctx, project.ID, cancelID, db.CronRequestKindCancel, runID, "operator")
	require.NoError(t, err)
	require.Equal(t, cancelID, canceled.Request.UID)
	require.Equal(t, runID, *canceled.Request.RunUID)
	require.Equal(t, db.CronRequestKindCancel, canceled.Request.Kind)
}

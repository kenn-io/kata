package client_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronDefinitionVersionSDKPreservesNumbers(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	workflow, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}, Options: jsontext.Value(`{"above":9007199254740993,"next":9007199254740994,"decimal":1.000000000000000000001}`)}})
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", WorkflowUID: workflow.UID}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Inspect"}, Overlap: "forbid", Catchup: "skip", Options: workflow.Definition.Options}})
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	jobs, err := client.ListCronJobVersionsWithResponse(t.Context(), &generated.ListCronJobVersionsRequestOptions{PathParams: &generated.ListCronJobVersionsPath{ProjectID: project.ID, CronUID: job.UID}})
	require.NoError(t, err)
	require.NotNil(t, jobs.JSON200)
	workflows, err := client.ListCronWorkflowVersionsWithResponse(t.Context(), &generated.ListCronWorkflowVersionsRequestOptions{PathParams: &generated.ListCronWorkflowVersionsPath{ProjectID: project.ID, CronUID: workflow.UID}})
	require.NoError(t, err)
	require.NotNil(t, workflows.JSON200)
	for _, body := range []any{jobs.JSON200, workflows.JSON200} {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		require.Contains(t, string(raw), `9007199254740993`)
		require.Contains(t, string(raw), `9007199254740994`)
		require.Contains(t, string(raw), `1.000000000000000000001`)
	}
}

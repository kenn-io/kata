package client_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// A run identity from NewCronUID is accepted by the daemon, and resubmitting
// the same identity and body after a lost response replays the observation
// instead of recording a second run or event.
func TestCronUIDRetryReplaysObservation(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	api, err := client.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	runUID, err := client.NewCronUID()
	require.NoError(t, err)
	actor := "worker"
	options := &generated.ObserveCronRunRequestOptions{
		PathParams: &generated.ObserveCronRunPath{ProjectID: project.ID, RunUID: runUID},
		Body: &generated.ObserveCronRunBody{
			Actor: &actor, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID,
			Status: generated.Running, Summary: generated.Summary{Version: 1},
		},
	}

	first, err := api.ObserveCronRunWithResponse(t.Context(), options)
	require.NoError(t, err)
	require.NotNilf(t, first.JSON200, "%s", first.Body)
	require.Equal(t, runUID, first.JSON200.Run.UID)
	require.False(t, first.JSON200.Replayed)
	afterFirst, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)

	retry, err := api.ObserveCronRunWithResponse(t.Context(), options)
	require.NoError(t, err)
	require.NotNilf(t, retry.JSON200, "%s", retry.Body)
	require.True(t, retry.JSON200.Replayed)
	require.Empty(t, retry.JSON200.Events)
	afterRetry, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, afterFirst, afterRetry, "a replayed observation emits no event")
	runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, runUID, runs[0].UID)
}

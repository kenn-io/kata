package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLISummaryRoundTrip(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Actor: "worker", Name: "Review", Definition: cronCLIJob(t).Definition.Native()})
	require.NoError(t, err)
	for _, summary := range []cron.Summary{
		{Version: 1},
		{Version: 1, Trigger: "request"},
		{Version: 1, Steps: []cron.SummaryStep{}},
		{Version: 1, Reason: "agent_quota", Usage: &cron.SummaryUsage{Model: "model-large", CostMicros: new(int64(0)), Currency: "USD"}, Steps: []cron.SummaryStep{{Key: "review", Status: "failed", ExitCode: new(int32(0)), StartedAt: "2026-11-03T01:00:00-08:00"}}, LogRef: "urn:example:run"},
	} {
		id := cronCLIUID(t)
		body := api.ObserveCronRunBody{JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: summary}
		for attempt := range 2 {
			raw, _, err := executeCronCLI(t, env.URL, strings.NewReader(string(mustCronJSON(t, body))), "run", "observe", id, "--json-input", "-")
			require.NoError(t, err)
			var result struct {
				Run      api.CronRun `json:"run"`
				Replayed bool        `json:"replayed"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &result))
			require.Equal(t, summary, result.Run.Summary)
			require.Equal(t, attempt == 1, result.Replayed)
		}
	}
}

func TestCronCLIReasonFilter(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "worker")
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Actor: "worker", Name: "Review", Definition: cronCLIJob(t).Definition.Native()})
	require.NoError(t, err)
	wanted := cronCLIUID(t)
	for _, id := range []string{wanted, cronCLIUID(t)} {
		reason := "timeout"
		if id == wanted {
			reason = "failed"
		}
		_, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: id, Actor: "worker", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "failed", Summary: cron.Summary{Version: 1, Reason: reason}})
		require.NoError(t, err)
	}
	raw, _, err := executeCronCLI(t, env.URL, strings.NewReader(""), "run", "list", "--reason", "failed", "--limit", "1")
	require.NoError(t, err)
	var result struct {
		Runs []api.CronRun `json:"runs"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &result))
	require.Len(t, result.Runs, 1)
	require.Equal(t, wanted, result.Runs[0].UID)
	_, _, err = executeCronCLI(t, env.URL, strings.NewReader(""), "run", "list", "--reason", "INVALID")
	require.Error(t, err)
}

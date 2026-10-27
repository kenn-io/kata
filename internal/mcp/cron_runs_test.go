package mcpserver

import (
	"encoding/json"
	"maps"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func cronObservation(id string, job map[string]any) map[string]any {
	return map[string]any{"uid": id, "job_uid": job["uid"], "definition_event_uid": job["definition_event_uid"], "expected_revision": 0, "status": "running", "summary": map[string]any{"version": 1, "message": "Synthetic observation"}}
}
func cronTestJob(t *testing.T, f cronFixture) map[string]any {
	t.Helper()
	return cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Observation fixture", "definition": cronDefinition()}, "job")
}
func TestCronRunObserveReplayAndRevision(t *testing.T) {
	f := newCronFixture(t)
	job := cronTestJob(t, f)
	id := cronNewUID(t, f.session)
	args := cronObservation(id, job)
	first := cronOK(t, f.session, "cron_run_observe", args)
	require.Equal(t, false, first["replayed"])
	require.Equal(t, float64(1), first["run"].(map[string]any)["revision"])
	replay := cronOK(t, f.session, "cron_run_observe", args)
	require.Equal(t, true, replay["replayed"])
	require.Equal(t, first["run"], replay["run"])
	args["status"] = "succeeded"
	require.True(t, cronCall(t, f.session, "cron_run_observe", args).IsError)
	args["expected_revision"] = 1
	updated := cronOK(t, f.session, "cron_run_observe", args)
	require.Equal(t, false, updated["replayed"])
	require.Equal(t, float64(2), updated["run"].(map[string]any)["revision"])
	shown := cronRecord(t, f.session, "cron_run", map[string]any{"uid": id}, "run")
	require.Equal(t, updated["run"], shown)
	require.Equal(t, "succeeded", shown["status"])
	delete(args, "expected_revision")
	require.True(t, cronCall(t, f.session, "cron_run_observe", args).IsError)
}
func TestCronRunDistinctSameOccurrence(t *testing.T) {
	f := newCronFixture(t)
	job := cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": cronNewUID(t, f.session), "name": "Fixture", "definition": cronDefinition()}, "job")
	for range 2 {
		args := cronObservation(cronNewUID(t, f.session), job)
		args["job_uid"] = job["uid"]
		args["definition_event_uid"] = job["definition_event_uid"]
		args["occurrence_key"] = "synthetic-occurrence"
		cronOK(t, f.session, "cron_run_observe", args)
	}
	runs := cronOK(t, f.session, "cron_runs", map[string]any{"job_uid": job["uid"]})["runs"].([]any)
	require.Len(t, runs, 2)
	require.NotEqual(t, runs[0].(map[string]any)["uid"], runs[1].(map[string]any)["uid"])
}
func TestCronRunTeammateAttribution(t *testing.T) {
	f := newCronFixture(t)
	job := cronTestJob(t, f)
	issue, _, err := f.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.project.ID, Title: "Attribution fixture", Author: "example-agent"})
	require.NoError(t, err)
	for _, test := range []struct {
		input *string
		want  any
	}{{nil, "default-worker"}, {new("worker-b"), "worker-b"}, {new(""), nil}} {
		args := cronObservation(cronNewUID(t, f.session), job)
		args["issue_uid"] = issue.UID
		if test.input != nil {
			args["teammate"] = *test.input
		}
		result := cronRecord(t, f.session, "cron_run_observe", args, "run")
		require.Equal(t, "example-agent", result["actor"])
		require.Equal(t, test.want, result["teammate"])
	}
	after, err := f.store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.Revision, after.Revision)
	require.Equal(t, issue.Owner, after.Owner)
	require.Equal(t, issue.Status, after.Status)
	bad := cronObservation(cronNewUID(t, f.session), job)
	bad["teammate"] = "invalid/handle"
	require.True(t, cronCall(t, f.session, "cron_run_observe", bad).IsError)
	bad = cronObservation(cronNewUID(t, f.session), job)
	bad["actor"] = "override"
	require.True(t, cronCall(t, f.session, "cron_run_observe", bad).IsError)
}
func TestCronRunPagination(t *testing.T) {
	f := newCronFixture(t)
	job := cronTestJob(t, f)
	var ids []string
	for range 3 {
		id := cronNewUID(t, f.session)
		ids = append(ids, id)
		cronOK(t, f.session, "cron_run_observe", cronObservation(id, job))
	}
	first := cronOK(t, f.session, "cron_runs", map[string]any{"limit": 2})
	require.Len(t, first["runs"], 2)
	require.Equal(t, ids[1], first["next_before_uid"])
	second := cronOK(t, f.session, "cron_runs", map[string]any{"limit": 2, "before_uid": first["next_before_uid"]})
	require.Len(t, second["runs"], 1)
	require.Equal(t, ids[0], second["runs"].([]any)[0].(map[string]any)["uid"])
	require.Empty(t, second["next_before_uid"])
	for _, limit := range []int{-1, 0, 101} {
		require.True(t, cronCall(t, f.session, "cron_runs", map[string]any{"limit": limit}).IsError)
	}
}
func TestCronRunExactNumbersAndTimes(t *testing.T) {
	f := newCronFixture(t)
	job := cronTestJob(t, f)
	args := cronObservation(cronNewUID(t, f.session), job)
	args["summary"] = map[string]any{"version": 1, "input_tokens": json.Number("9007199254740993")}
	args["started_at"] = "2026-10-18T12:00:00Z"
	args["ended_at"] = "2026-10-18T12:01:00Z"
	args["status"] = "succeeded"
	result := cronCall(t, f.session, "cron_run_observe", args)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, "9007199254740993")
	shown, err := f.store.CronRun(t.Context(), f.project.ID, args["uid"].(string))
	require.NoError(t, err)
	require.Equal(t, int64(9007199254740993), shown.Summary.InputTokens)
	require.Equal(t, time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC), *shown.StartedAt)
}

func TestCronRunProjectSelectionAndInvalidEvidence(t *testing.T) {
	f := newCronFixture(t)
	session := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "example-agent", Version: "test"})
	for _, name := range []string{"cron_capabilities", "cron_jobs", "cron_workflows", "cron_runs"} {
		require.True(t, cronCall(t, session, name, map[string]any{}).IsError, "%s cannot silently select a project", name)
		cronOK(t, session, name, map[string]any{"project": f.project.Name})
	}
	job := cronTestJob(t, f)
	for _, change := range []map[string]any{
		{"expected_revision": -1},
		{"status": "completed"},
		{"summary": map[string]any{"version": 2}},
		{"summary": map[string]any{"version": 1, "input_tokens": -1}},
		{"job_uid": "invalid"},
		{"definition_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
	} {
		args := cronObservation(cronNewUID(t, f.session), job)
		maps.Copy(args, change)
		require.True(t, cronCall(t, f.session, "cron_run_observe", args).IsError)
	}
	require.Empty(t, cronOK(t, f.session, "cron_runs", map[string]any{})["runs"])
}

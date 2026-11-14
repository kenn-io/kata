package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The shared store records separate executions even when their portable
// occurrence and issue references are identical. A logging retry identifies
// only its run UID, while changed evidence uses ordinary revision checks.
func TestCronIndependentRunObservations(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{
		ProjectID: project.ID, Name: "Review", Actor: "worker",
		Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "allow", Catchup: "skip"},
	})
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker"})
	require.NoError(t, err)
	body := map[string]any{"actor": "worker", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "occurrence_key": "manual:example", "issue_uid": issue.UID, "teammate": "runner", "executor_label": "example-executor", "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0}
	var first map[string]any
	for n := range 2 {
		id := cronHTTPUID(t)
		path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id)
		status, raw := cronHTTP(t, env, http.MethodPut, path, body)
		require.Equalf(t, http.StatusOK, status, "independent run %d: %s", n, raw)
		var response map[string]any
		require.NoError(t, json.Unmarshal(raw, &response))
		run := response["run"].(map[string]any)
		require.Equal(t, id, run["uid"])
		require.Equal(t, float64(1), run["revision"])
		require.NotContains(t, run, "snapshot")
		if n == 0 {
			first = run
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw = cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, http.StatusOK, status, "%s", raw)
			require.NoError(t, json.Unmarshal(raw, &response))
			require.Equal(t, true, response["replayed"])
			after, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "a retry creates no event")
			body["executor_label"] = "different-executor"
			status, raw = cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, http.StatusConflict, status, "%s", raw)
			body["executor_label"] = "example-executor"
		}
	}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, first["uid"])
	body["status"], body["expected_revision"] = "succeeded", 1
	status, raw := cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var response map[string]any
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, float64(2), response["run"].(map[string]any)["revision"])
	body["status"] = "failed"
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, http.StatusConflict, status, "stale different evidence: %s", raw)
	status, raw = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID), nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var listed struct {
		Runs []map[string]any `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(raw, &listed))
	require.Len(t, listed.Runs, 2)
}

func TestCronObservationBoundaries(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "allow", Catchup: "skip"}
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: definition})
	require.NoError(t, err)
	originalEvent := job.DefinitionEventUID
	definition.Action.Prompt = "Edited definition"
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker", Definition: definition})
	require.NoError(t, err)
	job, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: job.UID, Name: job.Name, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker", Definition: definition, Deleted: true})
	require.NoError(t, err)
	body := map[string]any{"actor": "worker", "job_uid": job.UID, "definition_event_uid": originalEvent, "teammate": "runner", "status": "succeeded", "summary": map[string]any{"version": 1, "message": "Buffered evidence"}, "expected_revision": 0}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t))
	status, raw := cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, http.StatusOK, status, "old revision after tombstone is still evidence: %s", raw)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	foreignPath := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", other.ID, cronHTTPUID(t))
	status, raw = cronHTTP(t, env, http.MethodPut, foreignPath, body)
	require.GreaterOrEqualf(t, status, 400, "foreign definition reference: %s", raw)
	require.Less(t, status, 500)
	body["definition_event_uid"] = cronHTTPUID(t)
	status, raw = cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), body)
	require.GreaterOrEqualf(t, status, 400, "invented revision reference: %s", raw)
	require.Less(t, status, 500)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "rejected foreign/unknown references create no events")
	body["definition_event_uid"] = originalEvent
	body["actor"] = "different-actor"
	status, raw = cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, http.StatusConflict, status, "run actor identity cannot be rewritten: %s", raw)
}

func TestCronRunObservationAcceptsExplicitNullTeammate(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "cron-null-teammate")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{
		ProjectID: project.ID, Name: "Observe", Actor: "worker", Definition: cronHTTPDefinition(t),
	})
	require.NoError(t, err)
	body := map[string]any{
		"actor": "worker", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID,
		"teammate": nil, "status": "running",
		"summary": map[string]any{"version": 1}, "expected_revision": 0,
	}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t))
	status, raw := cronHTTP(t, env, http.MethodPut, path, body)
	require.Equalf(t, http.StatusOK, status, "direct JSON null observation: %s", raw)

	runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Nil(t, runs[0].Teammate, "explicit null remains frozen as no teammate attribution")
}

type revokingRunObservationStore struct {
	db.Storage
	tokenID int64
}

func (s revokingRunObservationStore) ObserveCronRun(ctx context.Context, in db.ObserveCronRun) (db.CronRunObservationResult, error) {
	_, _, err := s.RevokeAPIToken(ctx, s.tokenID, db.BootstrapActor)
	if err != nil {
		return db.CronRunObservationResult{}, err
	}
	return s.Storage.ObserveCronRun(ctx, in)
}
func TestCronObservationRechecksRevokedActorToken(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	token, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-actor-token", Actor: "worker", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: revokingRunObservationStore{Storage: env.DB, tokenID: token.ID}, Auth: config.AuthConfig{Token: "tok", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	raw, err := json.Marshal(map[string]any{"actor": "supplied-actor", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "running", "summary": map[string]any{"version": 1}, "expected_revision": 0})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, httpServer.URL+fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), strings.NewReader(string(raw)))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer example-actor-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := httpServer.Client().Do(request)
	require.NoError(t, err)
	body := readClose(t, response)
	require.Equalf(t, 401, response.StatusCode, "revoked credential must not commit evidence: %s", body)
	runs, err := env.DB.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
	require.NoError(t, err)
	require.Empty(t, runs)
}

func TestCronRunStatsWindow(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/stats", project.ID)
	status, raw := cronHTTP(t, env, http.MethodGet, path+"?until=2026-11-01T00:00:00Z&bucket=day", nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Contains(t, result, "success_rate")
	require.Nil(t, result["success_rate"])
	require.Contains(t, result, "duration_seconds")
	require.Nil(t, result["duration_seconds"])
	require.Equal(t, "2026-10-02T00:00:00Z", result["window"].(map[string]any)["since"])
	status, _ = cronHTTP(t, env, http.MethodGet, path+"?job_uid=01J00000000000000000000010&workflow_uid=01J00000000000000000000020", nil)
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = cronHTTP(t, env, http.MethodGet, path+"?since=2026-11-01T00:00:00Z&until=2026-11-01T00:00:00Z", nil)
	require.Equal(t, http.StatusBadRequest, status)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Stats", Actor: "worker", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: cronHTTPUID(t), JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "failed", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodGet, path+"?job_uid="+strings.ToLower(job.UID)+"&bucket=day", nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	result = nil
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, float64(1), result["scanned"])
	require.Equal(t, job.UID, result["job_uid"])
	status, raw = cronHTTP(t, env, http.MethodGet, path+"?workflow_uid=01j00000000000000000000020", nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	result = nil
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, "01J00000000000000000000020", result["workflow_uid"])
	for _, query := range []string{
		"since=2025-11-01T00:00:00Z&until=2026-11-02T00:00:00Z",
		"since=0000-01-01T00:00:00Z&until=0000-01-02T00:00:00Z",
		"since=9999-12-30T23:59:59Z&until=9999-12-31T23:59:59Z",
	} {
		status, raw = cronHTTP(t, env, http.MethodGet, path+"?"+query, nil)
		require.Equalf(t, http.StatusOK, status, "%s: %s", query, raw)
	}
	for _, query := range []string{
		"since=2025-01-01T00:00:00Z&until=2026-11-01T00:00:00Z", "bucket=week", "timezone=Local", "timezone=Not%2FAZone",
		"since=0000-01-01T00:00:00%2B14:00&until=0000-01-02T00:00:00%2B14:00",
		"since=9999-12-30T00:00:00-14:00&until=9999-12-31T23:59:59-14:00",
		"until=0000-01-01T00:00:00Z", "since=9999-12-31T23:00:00-14:00&until=9999-12-31T23:30:00-14:00",
		"since=2d&until=2026-11-01T00:00:00Z",
	} {
		status, _ = cronHTTP(t, env, http.MethodGet, path+"?"+query, nil)
		require.Equal(t, http.StatusBadRequest, status, query)
	}
}

// A truncated store read returns the oldest retained instant to the handler.
type truncatedStatsStore struct{ db.Storage }

func (s truncatedStatsStore) CronRunStats(_ context.Context, _ db.CronRunStatsQuery) ([]cron.RunStatRow, bool, error) {
	newest := time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	rows := make([]cron.RunStatRow, cron.RunStatsScanLimit)
	for i := range rows {
		start := newest.Add(-time.Duration(i) * time.Second)
		rows[i] = cron.RunStatRow{Status: "failed", CreatedAt: newest.Add(24 * time.Hour), StartedAt: &start}
	}
	return rows, true, nil
}
func TestCronRunStatsEffectiveSinceAndTimezone(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	ts := startTestServer(t, daemon.ServerConfig{DB: truncatedStatsStore{env.DB}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+fmt.Sprintf("/api/v1/projects/%d/cron/runs/stats?since=2026-10-01T00:00:00Z&until=2026-11-02T00:00:00Z&bucket=day&timezone=America%%2FNew_York", project.ID), nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	raw := readClose(t, resp)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, true, result["truncated"])
	require.Equal(t, float64(cron.RunStatsScanLimit), result["scanned"])
	require.Equal(t, "2026-10-31T22:13:21Z", result["window"].(map[string]any)["since"])
	require.Equal(t, "2026-10-31", result["buckets"].([]any)[0].(map[string]any)["date"])
}

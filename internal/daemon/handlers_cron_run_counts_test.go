package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronRunCountsAPI(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Actor: "operator", Name: "Review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	workflow, _, err := env.DB.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: job.UID, Actor: "operator", Name: "Steps", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}}})
	require.NoError(t, err)
	for _, kind := range []string{"job", "workflow"} {
		in := db.ObserveCronRun{ProjectID: project.ID, UID: cronHTTPUID(t), Actor: "operator", WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}}
		if kind == "job" {
			in.JobUID = &job.UID
			in.DefinitionEventUID = &job.DefinitionEventUID
		}
		_, err = env.DB.ObserveCronRun(ctx, in)
		require.NoError(t, err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entry := db.CronTallyEntry{Kind: "job", UID: job.UID, CoverageStartedAt: at, LastPrunedAt: at, OldestCreatedAt: at, NewestCreatedAt: at, Counts: db.CronTallyCounts{Succeeded: 9007199254740993}}
	raw, err := json.Marshal(map[string]any{db.CronTallyPrefix + job.UID: db.CronTallyValue{Version: 1, Groups: map[string]db.CronTallyEntry{"job": entry}}})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(ctx, `UPDATE projects SET metadata=$1 WHERE id=$2`, string(raw), project.ID)
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/counts", project.ID)
	status, body := cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), "9007199254740993")
	var result db.CronRunCountsResult
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Groups, 2)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1}, result.Groups[0].Retained)
	require.Equal(t, entry.Counts, result.Groups[0].PrunedSinceTracking)
	require.Equal(t, db.CronRunRetainedCounts{Running: 1}, result.Groups[1].Retained)
	require.Nil(t, result.Groups[1].CoverageStartedAt)
	status, body = cronHTTP(t, env, http.MethodGet, path+"?workflow_uid="+strings.ToLower(workflow.UID), nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &result))
	require.Len(t, result.Groups, 1)
	require.Equal(t, "workflow", result.Groups[0].Kind, "counts filter is identity, not workflow membership")
	status, body = cronHTTP(t, env, http.MethodGet, path+"?job_uid="+cronHTTPUID(t), nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &result))
	require.Empty(t, result.Groups)
	for _, query := range []string{"?job_uid=invalid", "?job_uid=" + job.UID + "&workflow_uid=" + workflow.UID} {
		status, body = cronHTTP(t, env, http.MethodGet, path+query, nil)
		require.Equal(t, 400, status, string(body))
	}
	other := seedProject(t, env, "other-project")
	status, body = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/counts", other.ID), nil)
	require.Equal(t, 200, status, string(body))
	require.NoError(t, json.Unmarshal(body, &result))
	require.Empty(t, result.Groups)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "operator"})
	require.NoError(t, err)
	status, body = cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equal(t, 404, status, string(body))
}

// Counts use ordinary read authority and preserve the project visibility fence.
func TestCronRunCountsReadAuthority(t *testing.T) {
	for _, kind := range []string{"unauthenticated", "identity", "project-denied", "enrollment"} {
		t.Run(kind, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"), testenv.WithRequireTokenIdentity())
			project := seedProject(t, env, "counts-project")
			token, want := "", http.StatusUnauthorized
			if kind == "identity" || kind == "project-denied" {
				_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "example-counts-credential", Actor: "credential-worker", AdminActor: db.BootstrapActor})
				require.NoError(t, err)
				token, want = "example-counts-credential", http.StatusOK
			}
			if kind == "project-denied" {
				team, _, err := env.DB.CreateTeam(t.Context(), "project-readers", "admin")
				require.NoError(t, err)
				_, _, err = env.DB.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
				require.NoError(t, err)
				want = http.StatusNotFound
			}
			if kind == "enrollment" {
				_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "operator")
				require.NoError(t, err)
				enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "pull,push")
				token, want = enrollment.Token, http.StatusForbidden
			}
			response, raw := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/counts", project.ID), nil, bearer(token))
			require.Equal(t, want, response.StatusCode, string(raw))
		})
	}
}

func TestCronRunCountsUsesDatabaseIdentityToken(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newProjectAccessFixture(t, store, "cron-counts")
		path := fmt.Sprintf("/api/v1/projects/%d/cron/runs/counts", fixture.private.ID)
		status, _, body := fixture.request(t, http.MethodGet, path, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))

		status, _, body = fixture.request(t, http.MethodGet, path, "nonmember", nil, nil)
		require.Equal(t, http.StatusNotFound, status, string(body))
	})
}

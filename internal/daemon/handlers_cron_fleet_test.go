package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Contract: filters intersect before the page limit, on both run routes.
func TestCronFleetRunFilters(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	var want string
	for _, status := range []string{"failed", "succeeded"} {
		id := cronHTTPUID(t)
		body := map[string]any{"actor": "operator", "job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": status, "executor_label": "executor-a", "teammate": "runner", "summary": map[string]any{"version": 1}, "expected_revision": 0}
		code, raw := cronHTTP(t, env, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, id), body)
		require.Equalf(t, 200, code, "%s", raw)
		if status == "failed" {
			want = id
		}
	}
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID), "/api/v1/cron/runs"} {
		code, raw := cronHTTP(t, env, http.MethodGet, path+"?status=failed&status=failed&executor_label=executor-a&teammate=runner&since=2000-01-01T00:00:00Z&limit=1", nil)
		require.Equalf(t, 200, code, "%s", raw)
		var out struct {
			Runs     []db.CronRun   `json:"runs"`
			Filters  map[string]any `json:"filters"`
			Projects []struct {
				ID   int64  `json:"id"`
				UID  string `json:"uid"`
				Name string `json:"name"`
			} `json:"projects"`
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Len(t, out.Runs, 1)
		require.Equal(t, want, out.Runs[0].UID)
		require.Equal(t, []any{"failed"}, out.Filters["status"])
		if path == "/api/v1/cron/runs" {
			require.Len(t, out.Projects, 1)
			require.Equal(t, project.ID, out.Projects[0].ID)
			require.Equal(t, project.UID, out.Projects[0].UID)
			require.Equal(t, project.Name, out.Projects[0].Name)
		}
	}
}

func TestCronFleetProjectAuthorization(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		var publicRunUID, privateRunUID string
		for _, p := range []db.Project{f.public, f.private} {
			job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "member", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			runUID := cronHTTPUID(t)
			_, err = store.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "member", Status: "failed", Summary: cron.Summary{Version: 1}})
			require.NoError(t, err)
			if p.ID == f.public.ID {
				publicRunUID = runUID
			} else {
				privateRunUID = runUID
			}
		}
		for _, path := range []string{"/api/v1/cron/jobs", "/api/v1/cron/runs"} {
			code, _, raw := f.request(t, http.MethodGet, path, "nonmember", nil, nil)
			require.Equalf(t, 200, code, "%s", raw)
			require.NotContains(t, string(raw), f.private.UID)
			if path == "/api/v1/cron/runs" {
				var out struct {
					Runs []struct {
						UID string `json:"uid"`
					} `json:"runs"`
				}
				require.NoError(t, json.Unmarshal(raw, &out))
				uids := make([]string, 0, len(out.Runs))
				for _, run := range out.Runs {
					uids = append(uids, run.UID)
				}
				require.Contains(t, uids, publicRunUID)
				require.NotContains(t, uids, privateRunUID)
			}
			code, _, raw = f.request(t, http.MethodGet, fmt.Sprintf("%s?project_ids=%d", path, f.private.ID), "nonmember", nil, nil)
			require.Equalf(t, 404, code, "%s", raw)
			for _, bad := range []string{"", "0", "-1", "garbage"} {
				code, _, raw = f.request(t, http.MethodGet, path+"?project_ids="+bad, "admin", nil, nil)
				require.Equalf(t, 400, code, "%s", raw)
			}
		}
		code, _, raw := f.request(t, http.MethodGet, "/api/v1/cron/jobs", "nonmember", nil, nil)
		require.Equal(t, 200, code)
		require.Contains(t, string(raw), f.public.UID)
	})
}

func TestCronFleetHostAuthorization(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	host := &capabilitiesReadHost{projectID: project.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, path := range []string{"/api/v1/cron/jobs", "/api/v1/cron/runs"} {
		for _, explicit := range []bool{true, false} {
			target := path
			if explicit {
				target += fmt.Sprintf("?project_ids=%d", project.ID)
			}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			want := 404
			if explicit {
				want = 200
			}
			require.Equalf(t, want, response.Code, "%s", response.Body.String())
		}
	}
	require.Len(t, host.calls, 4)
}

func TestCronFleetFilterValidation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, path := range []string{"/api/v1/cron/runs", fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID)} {
		for _, query := range []string{"status=bogus", "status=failed&status=bogus", "status=", "issue_uid=bad", "executor_label=%20", "teammate=bad/name", "since=yesterday", "since=", "executor_label=a&executor_label=b"} {
			code, raw := cronHTTP(t, env, http.MethodGet, path+"?"+query, nil)
			require.Equalf(t, 400, code, "%s: %s", query, raw)
		}
	}
}

func TestCronFleetIssueScopedDenial(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project := seedProject(t, env, "example-project")
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	expires := time.Now().UTC().Add(time.Hour)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/cron/jobs", "/api/v1/cron/runs", fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID), fmt.Sprintf("/api/v1/projects/%d/cron/runs", project.ID)} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+path, nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer worker-token")
		response, err := env.HTTP.Do(request)
		require.NoError(t, err)
		raw := readClose(t, response)
		require.Equalf(t, 403, response.StatusCode, "%s", raw)
		require.Contains(t, string(raw), "scoped_operation_forbidden")
	}
}

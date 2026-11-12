package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsview"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronUsageReadDoesNotChangeRunOrEvents(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Example", Actor: "worker", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Example"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example"}, Overlap: "allow", Catchup: "skip"}})
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "example", SessionID: "session"}}
	uid := cronHTTPUID(t)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: uid, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}})
	require.NoError(t, err)
	before, err := env.DB.CronRun(t.Context(), project.ID, uid)
	require.NoError(t, err)
	cursor, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"session_id":"session","has_cost":true,"has_token_data":true,"cost":{"microdollars":0},"cost_source":"reported"}`))
	}))
	t.Cleanup(upstream.Close)
	svc, err := agentsview.NewService(config.ResolvedAgentsView{URL: upstream.URL, BrowserURL: upstream.URL}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: svc})
	t.Cleanup(func() { _ = server.Close() })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, uid), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"state":"complete"`)
	require.Contains(t, out.Body.String(), `"cost_microdollars":0`)
	require.Contains(t, out.Header().Get("Cache-Control"), "no-store")
	require.EqualValues(t, 1, calls.Load())
	after, err := env.DB.CronRun(t.Context(), project.ID, uid)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterCursor, err := env.DB.UIEventCursor(t.Context())
	require.NoError(t, err)
	require.Equal(t, cursor, afterCursor)
}

func TestCronUsageDisabledAndAnonymous(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for _, tc := range []struct {
		name     string
		readonly bool
		want     int
		code     string
	}{{"disabled", false, 503, "usage_source_disabled"}, {"anonymous", true, 403, "usage_forbidden"}} {
		t.Run(tc.name, func(t *testing.T) {
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, InsecureReadonly: tc.readonly})
			defer func() { _ = server.Close() }()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, cronHTTPUID(t)), nil)
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, req)
			require.Equal(t, tc.want, out.Code, out.Body.String())
			require.Contains(t, out.Body.String(), tc.code)
		})
	}
}

func TestDoctorReportsAgentsViewOff(t *testing.T) {
	env := testenv.New(t)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB})
	defer func() { _ = server.Close() }()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctor", nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"agentsview":{"state":"off"}`)
}

func TestCronUsageHostAuthorizationIsProjectScoped(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	host := &capabilitiesReadHost{projectID: project.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
	defer func() { _ = server.Close() }()
	for _, tc := range []struct {
		id   int64
		want int
	}{{project.ID, 503}, {other.ID, 404}} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", tc.id, cronHTTPUID(t)), nil)
		req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		require.Equal(t, tc.want, out.Code, out.Body.String())
	}
	require.Len(t, host.calls, 2)
	require.Equal(t, "getCronRunUsage", host.calls[0].ID)
	require.False(t, host.calls[0].Policy.Mutation)
}

func TestCronUsageAnonymousDoesNotContactEnabledSource(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(upstream.Close)
	service, err := agentsview.NewService(config.ResolvedAgentsView{URL: upstream.URL, BrowserURL: upstream.URL}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, InsecureReadonly: true, AgentsView: service})
	defer func() { _ = server.Close() }()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, cronHTTPUID(t)), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusForbidden, out.Code, out.Body.String())
	require.Zero(t, calls.Load())
}

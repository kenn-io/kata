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
	sessions := []cron.Session{{Agent: "example", SessionID: "session"}}
	uid := cronHTTPUID(t)
	_, err = env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: project.ID, UID: uid, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}})
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

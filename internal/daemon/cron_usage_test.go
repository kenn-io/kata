package daemon_test

import (
	"context"
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
	assertCronUsageReadDoesNotChangeRunOrEvents(t, t.Context(), env.DB)
}

func assertCronUsageReadDoesNotChangeRunOrEvents(t *testing.T, ctx context.Context, store db.Storage) {
	t.Helper()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Example", Actor: "worker", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Example"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example"}, Overlap: "allow", Catchup: "skip"}})
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "example", SessionID: "session"}}
	uid := cronHTTPUID(t)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: uid, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}})
	require.NoError(t, err)
	before, err := store.CronRun(ctx, project.ID, uid)
	require.NoError(t, err)
	cursor, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"session_id":"session","has_cost":true,"has_token_data":true,"cost":{"microdollars":0},"cost_source":"reported"}`))
	}))
	t.Cleanup(upstream.Close)
	svc, err := agentsview.NewService(config.ResolvedAgentsView{URL: upstream.URL, BrowserURL: upstream.URL}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, AgentsView: svc})
	t.Cleanup(func() { _ = server.Close() })
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, uid), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"state":"complete"`)
	require.Contains(t, out.Body.String(), `"cost_microdollars":0`)
	require.Contains(t, out.Header().Get("Cache-Control"), "no-store")
	require.EqualValues(t, 1, calls.Load())
	after, err := store.CronRun(ctx, project.ID, uid)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterCursor, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, afterCursor)
}

func TestCronUsageDisabledAndAnonymous(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	service, err := agentsview.NewService(config.ResolvedAgentsView{}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: service})
	defer func() { _ = server.Close() }()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, cronHTTPUID(t)), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusServiceUnavailable, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), "usage_source_disabled")
}

func TestDoctorReportsAgentsViewOff(t *testing.T) {
	env := testenv.New(t)
	service, err := agentsview.NewService(config.ResolvedAgentsView{}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: service})
	defer func() { _ = server.Close() }()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctor", nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"agentsview":{"state":"off"}`)
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
	require.Contains(t, out.Body.String(), "usage_forbidden")
	require.Zero(t, calls.Load())
}

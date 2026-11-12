package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	refresh := assertCronUsageReadDoesNotChangeRunOrEvents(t, t.Context(), env.DB)
	// The handler check uses the real service clock; service timing is tested separately.
	timer := time.NewTimer(11 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	refresh()
}

func assertCronUsageReadDoesNotChangeRunOrEvents(t *testing.T, ctx context.Context, store db.Storage) func() {
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
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok)
	cursor, err := uiStore.UIEventCursor(ctx)
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
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, strings.ToLower(uid)), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	require.Contains(t, out.Body.String(), `"state":"complete"`)
	require.Contains(t, out.Body.String(), `"run_uid":"`+before.UID+`"`)
	require.Contains(t, out.Body.String(), `"cost_microdollars":0`)
	require.Contains(t, out.Header().Get("Cache-Control"), "no-store")
	require.EqualValues(t, 1, calls.Load())
	after, err := store.CronRun(ctx, project.ID, uid)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterCursor, err := uiStore.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, afterCursor)
	return func() {
		cached := httptest.NewRecorder()
		server.Handler().ServeHTTP(cached, httptest.NewRequestWithContext(ctx, http.MethodGet, req.URL.String(), nil))
		require.Equal(t, http.StatusOK, cached.Code, cached.Body.String())
		require.EqualValues(t, 1, calls.Load())
		refreshed := httptest.NewRecorder()
		server.Handler().ServeHTTP(refreshed, httptest.NewRequestWithContext(ctx, http.MethodGet, req.URL.String()+"?refresh=true", nil))
		require.Equal(t, http.StatusOK, refreshed.Code, refreshed.Body.String())
		require.EqualValues(t, 2, calls.Load())
	}
}

func TestCronUsageDisabled(t *testing.T) {
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

func TestDoctorAgentsViewSource(t *testing.T) {
	env := testenv.New(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"session_not_found","message":"not found"}}`))
	}))
	t.Cleanup(upstream.Close)
	for _, tc := range []struct {
		name, url, state string
		calls            int32
	}{
		{"off", "", "off", 0},
		{"on", upstream.URL, "ok", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &doctorSink{}
			service, err := agentsview.NewService(config.ResolvedAgentsView{URL: tc.url, BrowserURL: tc.url}, false)
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: service, Hooks: sink})
			t.Cleanup(func() { _ = server.Close() })
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/doctor", nil))
			require.Equal(t, http.StatusOK, out.Code, out.Body.String())
			require.Contains(t, out.Body.String(), `"agentsview":{"state":"`+tc.state+`"}`)
			require.Contains(t, out.Body.String(), `"available":true`)
			require.EqualValues(t, 1, sink.reads.Load())
			require.Equal(t, tc.calls, calls.Load())
		})
	}
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

func TestCronUsageCapabilitiesAndHTTPErrors(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Example", Actor: "worker", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Example"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example"}, Overlap: "allow", Catchup: "skip"}})
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "example", SessionID: "one"}, {Agent: "example", SessionID: "two"}}
	uid := cronHTTPUID(t)
	_, err = env.DB.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: uid, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}})
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "one"
		if strings.Contains(r.URL.Path, "/two/") {
			id = "two"
		}
		_, _ = fmt.Fprintf(w, `{"session_id":%q,"has_cost":true,"has_token_data":true,"cost":{"microdollars":9223372036854775807},"cost_source":"reported"}`, id)
	}))
	t.Cleanup(upstream.Close)
	enabled, err := agentsview.NewService(config.ResolvedAgentsView{URL: upstream.URL, BrowserURL: upstream.URL}, false)
	require.NoError(t, err)
	disabled, err := agentsview.NewService(config.ResolvedAgentsView{}, false)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, source string
		service      *agentsview.Service
	}{{"off", "none", disabled}, {"on", "agentsview", enabled}} {
		t.Run(tc.name, func(t *testing.T) {
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: tc.service})
			t.Cleanup(func() { _ = server.Close() })
			request := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil)
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, request)
			require.Equal(t, http.StatusOK, out.Code, out.Body.String())
			require.Contains(t, out.Body.String(), `"source":"`+tc.source+`"`)
		})
	}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, AgentsView: enabled})
	t.Cleanup(func() { _ = server.Close() })
	for _, tc := range []struct {
		uid    string
		status int
		code   string
	}{{uid, http.StatusBadGateway, "usage_total_overflow"}, {cronHTTPUID(t), http.StatusNotFound, ""}} {
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, tc.uid), nil)
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, request)
		require.Equal(t, tc.status, out.Code, out.Body.String())
		if tc.code != "" {
			require.Contains(t, out.Body.String(), tc.code)
		}
	}
}

package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
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
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	_, _, err = executeCronCLI(t, server.URL, strings.NewReader(""), "run", "list", "--reason", "INVALID")
	var cliErr *cliError
	require.ErrorAs(t, err, &cliErr)
	require.Equal(t, kindValidation, cliErr.Kind)
	require.Equal(t, ExitValidation, cliErr.ExitCode)
	require.Contains(t, cliErr.Message, "invalid reason code")
	require.Zero(t, requests.Load(), "invalid reasons must fail before dialing the daemon")
}

func TestCronCLIReasonOmittedWhenUnset(t *testing.T) {
	env := testenv.New(t)
	_, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	queries := make(chan bool, 2)
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			probes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/cron/runs") {
			queries <- r.URL.Query().Has("reason")
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	_, _, err = executeCronCLI(t, server.URL, strings.NewReader(""), "run", "list")
	require.NoError(t, err)
	select {
	case present := <-queries:
		require.False(t, present)
	default:
		t.Fatal("run list was not requested")
	}
	require.Zero(t, probes.Load())
}

// A selector must never silently become an unfiltered read on an older daemon.
func TestCronRunListReasonRefusesOldDaemon(t *testing.T) {
	for _, health := range []string{`{"api_schema_version":"0.40.0"}`, `{"api_schema_version":""}`, `{}`, `{"api_schema_version":"0.42.0"}`} {
		t.Run(health, func(t *testing.T) {
			env := testenv.New(t)
			_, err := env.DB.CreateProject(t.Context(), "spoke-project")
			require.NoError(t, err)
			target, err := url.Parse(env.URL)
			require.NoError(t, err)
			proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
			var reads, probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					probes.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(health))
					return
				}
				if strings.HasSuffix(r.URL.Path, "/cron/runs") {
					reads.Add(1)
					assert.Equal(t, "failed", r.URL.Query().Get("reason"))
				}
				proxy.ServeHTTP(w, r)
			}))
			defer server.Close()
			_, _, err = executeCronCLI(t, server.URL, strings.NewReader(""), "run", "list", "--reason", "failed")
			if strings.Contains(health, "0.42.0") {
				require.NoError(t, err)
				require.EqualValues(t, 1, reads.Load())
			} else {
				require.Error(t, err)
				var cliErr *cliError
				require.ErrorAs(t, err, &cliErr)
				require.Equal(t, "daemon_api_too_old", cliErr.Code)
				require.Zero(t, reads.Load())
			}
			require.EqualValues(t, 1, probes.Load())
		})
	}
}

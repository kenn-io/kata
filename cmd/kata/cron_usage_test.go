package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsview"
)

func TestCronUsageOutputStatesAndSanitization(t *testing.T) {
	amount := int64(2410000)
	step := "main"
	usage := agentsview.RunUsage{Total: agentsview.Total{State: "partial", CostMicrodollars: &amount, Estimated: true}, SessionsIncomplete: true, Sessions: []agentsview.RunSession{
		{Agent: "example", SessionID: "one", Step: &step, SessionUsage: agentsview.SessionUsage{State: "priced", CostMicrodollars: &amount, CostSource: "computed"}},
		{Agent: "example", SessionID: "two", SessionUsage: agentsview.SessionUsage{State: "unpriced", UnpricedModels: []string{"model\x1b[31m"}}},
		{Agent: "example", SessionID: "three", SessionUsage: agentsview.SessionUsage{State: "not_found"}},
		{Agent: "example", SessionID: "four", SessionUsage: agentsview.SessionUsage{State: "ambiguous", Candidates: []string{"archive~id"}}},
		{Agent: "example", SessionID: "five", SessionUsage: agentsview.SessionUsage{State: "no_usage"}},
		{Agent: "example", SessionID: "six", SessionUsage: agentsview.SessionUsage{State: "unauthorized"}},
		{Agent: "example", SessionID: "seven", SessionUsage: agentsview.SessionUsage{State: "unavailable"}},
	}}
	for _, tc := range []struct {
		value     int64
		estimated bool
		want      string
	}{{0, false, "$0.00"}, {0, true, "~$0.00"}, {1, false, "<$0.01"}, {4999, true, "~<$0.01"}, {5000, false, "$0.01"}} {
		require.Equal(t, tc.want, usageAmount(tc.value, tc.estimated))
	}
	var out bytes.Buffer
	require.NoError(t, writeCronUsage(&out, usage))
	for _, text := range []string{"~$2.41+ (6 of 7 sessions not priced)", "session list is incomplete", "unpriced", "not in AgentsView yet", "ambiguous session", "no usage recorded", "AgentsView rejected Kata's token", "AgentsView unavailable"} {
		require.Contains(t, out.String(), text)
	}
	require.NotContains(t, out.String(), "\x1b")
}

func TestCronUsageChecksCapabilitiesBeforeUsage(t *testing.T) {
	var usageCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		case "/api/v1/projects/7/cron/capabilities":
			_, _ = io.WriteString(w, `{"project_uid":"id","event_features":[],"usage":{"source":"none"}}`)
		default:
			usageCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	_, _, err := executeCronCLI(t, server.URL, nil, "run", "usage", cronCLIUID(t))
	require.ErrorContains(t, err, "usage source is disabled")
	require.Zero(t, usageCalls.Load())
}

func TestCronUsageCommandOutputAndRefresh(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		case "/api/v1/projects/7/cron/capabilities":
			_, _ = io.WriteString(w, `{"project_uid":"id","event_features":[],"usage":{"source":"agentsview"}}`)
		default:
			queries = append(queries, r.URL.RawQuery)
			_, _ = io.WriteString(w, `{"run_uid":"fixture","state":"complete","cost_microdollars":1000000,"estimated":false,"sessions_incomplete":false,"fetched_at":"2026-01-01T00:00:00Z","sessions":[]}`)
		}
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"human", nil, "Cost: $1.00"},
		{"json", []string{"--json"}, `"cost_microdollars":1000000`},
		{"quiet", []string{"--quiet"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			command := newRootCmd()
			var out bytes.Buffer
			command.SetOut(&out)
			command.SetErr(io.Discard)
			command.SetContext(contextWithBaseURL(t.Context(), server.URL))
			args := append([]string{"--project", "spoke-project"}, tc.flags...)
			command.SetArgs(append(args, "cron", "run", "usage", cronCLIUID(t), "--refresh"))
			require.NoError(t, command.Execute())
			if tc.want == "" {
				require.Empty(t, out.String())
			} else {
				require.Contains(t, out.String(), tc.want)
			}
		})
	}
	require.Equal(t, []string{"refresh=true", "refresh=true", "refresh=true"}, queries)
}

func TestCronUsageHumanTotalStates(t *testing.T) {
	amount := int64(1250000)
	for _, tc := range []struct {
		total agentsview.Total
		want  string
	}{
		{agentsview.Total{State: "complete", CostMicrodollars: &amount}, "Cost: $1.25\n"},
		{agentsview.Total{State: "unknown"}, "Cost: unknown\n"},
		{agentsview.Total{State: "no_sessions"}, "Cost: no sessions\n"},
	} {
		t.Run(tc.total.State, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, writeCronUsage(&out, agentsview.RunUsage{Total: tc.total}))
			require.Equal(t, tc.want, out.String())
		})
	}
}

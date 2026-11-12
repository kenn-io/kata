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

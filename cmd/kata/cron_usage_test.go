package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronUsageOutputStatesAndSanitization(t *testing.T) {
	// A newer daemon may return states this client does not recognize.
	for _, tc := range []struct {
		name  string
		usage generated.RunUsage
	}{
		{"run", generated.RunUsage{State: "future_run_state"}},
		{"session", generated.RunUsage{State: "unknown", Sessions: []generated.RunSession{{Agent: "example", SessionID: "fixture", State: "future_session_state"}}}},
	} {
		t.Run("newer daemon "+tc.name, func(t *testing.T) {
			require.Error(t, validateCronUsage(tc.usage))
		})
	}
	amount := int64(2410000)
	step := "main"
	source := "computed"
	usage := generated.RunUsage{State: "partial", CostMicrodollars: &amount, Estimated: true, SessionsIncomplete: true, Sessions: []generated.RunSession{
		{Agent: "example", SessionID: "one", Step: &step, State: "priced", CostMicrodollars: &amount, CostSource: &source},
		{Agent: "example", SessionID: "two", State: "unpriced", UnpricedModels: []string{"model\x1b[31m"}},
		{Agent: "example", SessionID: "three", State: "not_found"},
		{Agent: "example", SessionID: "four", State: "ambiguous", Candidates: []string{"archive~id"}},
		{Agent: "example", SessionID: "five", State: "no_usage"},
		{Agent: "example", SessionID: "six", State: "unauthorized"},
		{Agent: "example", SessionID: "seven", State: "unavailable"},
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
	for _, tc := range []struct {
		usage generated.RunUsage
		want  string
	}{
		{generated.RunUsage{State: "complete", CostMicrodollars: &amount, Sessions: []generated.RunSession{{Agent: "example", SessionID: "id", State: "priced", CostMicrodollars: &amount}}}, "Cost: $2.41\n"},
		{generated.RunUsage{State: "partial", CostMicrodollars: &amount, SessionsIncomplete: true, Sessions: []generated.RunSession{{Agent: "example", SessionID: "id", State: "priced", CostMicrodollars: &amount}}}, "Cost: $2.41+ (0 of 1 sessions not priced)\n"},
		{generated.RunUsage{State: "unknown", Sessions: []generated.RunSession{{Agent: "example", SessionID: "id", State: "not_found"}}}, "Cost: unknown\n"},
		{generated.RunUsage{State: "no_sessions"}, "Cost: no sessions\n"},
	} {
		t.Run(string(tc.usage.State), func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, writeCronUsage(&output, tc.usage))
			require.True(t, strings.HasPrefix(output.String(), tc.want))
		})
	}
}

func TestCronUsageCommandOutputAndRefresh(t *testing.T) {
	var queries []string
	var disabled atomic.Bool
	const smallBody = `{"run_uid":"fixture","state":"complete","cost_microdollars":1000000,"estimated":false,"sessions_incomplete":false,"fetched_at":"2026-01-01T00:00:00Z","sessions":[{"agent":"example","session_id":"id","state":"priced","cost_microdollars":1000000,"cost_source":"reported"}]}`
	// Each session can be produced by an upstream response below its 1 MiB cap,
	// so a valid aggregate of 64 of them exceeds 2 MiB.
	sessions := make([]string, 64)
	for i := range sessions {
		sessions[i] = fmt.Sprintf(`{"agent":"example","session_id":"id%d","state":"unpriced","unpriced_models":[%q]}`, i, strings.Repeat("m", 40<<10))
	}
	largeBody := `{"run_uid":"fixture","state":"unknown","estimated":false,"sessions_incomplete":false,"fetched_at":"2026-01-01T00:00:00Z","sessions":[` + strings.Join(sessions, ",") + `]}`
	require.Greater(t, len(largeBody), 2<<20)
	var usageBody atomic.Value
	usageBody.Store(smallBody)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		case "/api/v1/projects/7/cron/capabilities":
			source := "agentsview"
			if disabled.Load() {
				source = "none"
			}
			_, _ = fmt.Fprintf(w, `{"project_uid":"id","event_features":[],"usage":{"source":%q}}`, source)
		default:
			queries = append(queries, r.URL.RawQuery)
			_, _ = io.WriteString(w, usageBody.Load().(string))
		}
	}))
	t.Cleanup(server.Close)
	cases := []struct {
		name  string
		flags []string
		want  string
		body  string
	}{
		{"human", nil, "Cost: $1.00", smallBody},
		{"json", []string{"--json"}, `"cost_microdollars":1000000`, smallBody},
		{"quiet", []string{"--quiet"}, "", smallBody},
		{"agent", []string{"--agent"}, "run_uid=fixture", smallBody},
		{"large human", nil, "id63", largeBody},
		{"large json", []string{"--json"}, `"session_id":"id63"`, largeBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usageBody.Store(tc.body)
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
			if tc.name == "agent" {
				require.True(t, strings.HasPrefix(out.String(), "OK "))
				require.Contains(t, out.String(), "state=complete")
				require.Contains(t, out.String(), "example id")
			}
		})
	}
	require.Len(t, queries, len(cases))
	for _, query := range queries {
		require.Equal(t, "refresh=true", query)
	}
	t.Run("source none", func(t *testing.T) {
		disabled.Store(true)
		_, _, err := executeCronCLI(t, server.URL, nil, "run", "usage", cronCLIUID(t))
		require.ErrorContains(t, err, "usage source is disabled")
		require.Len(t, queries, len(cases))
	})
}

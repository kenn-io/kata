package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Run in isolation with -run '^$' -bench '^BenchmarkMoveIssuesBatch2000$'
// -benchtime=1x -count=3. Seeding is excluded; HTTP, claims and writes are timed.
// Compare repeated measurements to the retained local/spoke throughput baseline.
func BenchmarkMoveIssuesBatch2000(b *testing.B) {
	for _, spoke := range []bool{false, true} {
		b.Run(fmt.Sprintf("spoke=%t", spoke), func(b *testing.B) {
			b.StopTimer()
			var total time.Duration
			for range b.N {
				total += moveIssuesBatch2000(b, spoke, func(run func()) { b.StartTimer(); run(); b.StopTimer() })
			}
			b.ReportMetric(float64(total.Nanoseconds())/float64(b.N*2000), "ns/issue")
		})
	}
}

func moveIssuesBatch2000(t testing.TB, spoke bool, measure func(func())) time.Duration {
	t.Helper()
	env := testenv.New(t)
	source, err := env.DB.CreateProject(t.Context(), "routing-source")
	require.NoError(t, err)
	var targets []db.Project
	for _, name := range []string{"routing-a", "routing-b"} {
		p, err := env.DB.CreateProject(t.Context(), name)
		require.NoError(t, err)
		targets = append(targets, p)
	}
	if spoke {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			//nolint:kennlint // sleeptest: Synthetic hub latency is the measured workload for the spoke benchmark.
			time.Sleep(20 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{HubNow: time.Now().UTC()})
		}))
		defer hub.Close()
		for _, p := range append([]db.Project{source}, targets...) {
			_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: p.ID, HubProjectUID: p.UID, Actor: "worker", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			require.NoError(t, config.WriteFederationCredential(p.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: p.ID, Token: "synthetic-hub-token", Actor: "worker"}))
		}
	}
	var issues []db.Issue
	for n := range 2000 {
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: fmt.Sprintf("Route archived engineering note %d", n), Body: strings.Repeat("Synthetic implementation notes and follow-up context. ", 20), Author: "worker", Labels: []string{"triaged", "routing"}})
		require.NoError(t, err)
		issues = append(issues, issue)
	}
	var items []map[string]any
	for n, issue := range issues {
		items = append(items, map[string]any{"ref": issue.UID, "to_project_uid": targets[n%2].UID})
	}
	var elapsed time.Duration
	measure(func() {
		started := time.Now()
		start := 0
		for start < len(items) {
			raw, err := json.Marshal(map[string]any{"actor": "worker", "comment": "routed", "items": items, "start_index": start})
			require.NoError(t, err)
			reply := postMoveBatch(t, env, string(raw))
			require.Greater(t, reply.NextIndex, start)
			require.Len(t, reply.Results, reply.NextIndex-start)
			for _, r := range reply.Results {
				require.Equal(t, "moved", r.Result)
			}
			start = reply.NextIndex
		}

		elapsed = time.Since(started)
	})
	t.Logf("PERF spoke=%t issues=2000 elapsed=%s microseconds_per_issue=%d", spoke, elapsed, elapsed.Microseconds()/2000)
	return elapsed
}

package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// This fixture also runs unmodified on the base branch. The timed region
// excludes seeding and compilation and includes all HTTP and claim work.
func TestMoveIssuesBatchPerf2000(t *testing.T) {
	if testing.Short() {
		t.Skip("2000-issue throughput measurement")
	}
	for _, spoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("spoke=%t", spoke), func(t *testing.T) {
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
					//nolint:kennlint // sleeptest: Synthetic hub latency is the measured workload for the spoke throughput gate.
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
			started := time.Now()
			if os.Getenv("KATA_MOVE_PERF_LEGACY") == "1" {
				for n, issue := range issues {
					// Model the legacy CLI's show, move and follow-up comment requests.
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s", env.URL, source.ID, issue.UID), nil)
					require.NoError(t, err)
					resp, err := env.HTTP.Do(req)
					require.NoError(t, err)
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					require.Equal(t, http.StatusOK, resp.StatusCode)
					response := doPostWithIfMatch(t, env, moveURL(env, source.ID, issue.UID), fmt.Sprintf(`{"actor":"worker","single":true,"to_project_uid":%q}`, targets[n%2].UID), fmt.Sprintf(`"rev-%d"`, issue.Revision))
					raw, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					response = doPostWithIfMatch(t, env, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/comments", env.URL, targets[n%2].ID, issue.UID), `{"actor":"worker","body":"routed"}`, "")
					raw, err = io.ReadAll(response.Body)
					_ = response.Body.Close()
					require.NoError(t, err)
					require.Less(t, response.StatusCode, 300, string(raw))
				}
			} else {
				var items []map[string]any
				for n, issue := range issues {
					items = append(items, map[string]any{"ref": issue.UID, "to_project_uid": targets[n%2].UID})
				}
				start := 0
				for start < len(items) {
					raw, err := json.Marshal(map[string]any{"actor": "worker", "comment": "routed", "items": items, "start_index": start})
					require.NoError(t, err)
					reply := postMoveBatch(t, env, string(raw))
					require.Greater(t, reply.NextIndex, start)
					for _, r := range reply.Results {
						require.Equal(t, "moved", r.Result)
					}
					start = reply.NextIndex
				}
			}
			elapsed := time.Since(started)
			t.Logf("PERF legacy=%t spoke=%t issues=2000 elapsed=%s microseconds_per_issue=%d", os.Getenv("KATA_MOVE_PERF_LEGACY") == "1", spoke, elapsed, elapsed.Microseconds()/2000)
			if os.Getenv("KATA_MOVE_PERF_LEGACY") != "1" {
				limit := 15 * time.Second
				if spoke {
					limit = 60 * time.Second
				}
				require.Less(t, elapsed, limit)
			}
		})
	}
}

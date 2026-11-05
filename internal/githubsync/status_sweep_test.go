package githubsync

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestGitHubStatusSweepRequestCount(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			h := newRunnerHarness(t)
			for n := 1; n <= 100; n++ {
				h.fetcher.issues = append(h.fetcher.issues, testIssue(int64(1000+n), n, "Example task", h.now.Add(-time.Hour)))
			}
			_, err := h.runner.RunOnce(h.ctx, h.binding.ID)
			require.NoError(t, err)
			cfg, err := DecodeConfig(h.binding.Config)
			require.NoError(t, err)
			cfg.StatusSync = "two-way"
			raw, err := EncodeConfig(cfg)
			require.NoError(t, err)
			b, err := h.db.UpsertIssueSyncBinding(h.ctx, db.UpsertIssueSyncBindingParams{ProjectID: h.binding.ProjectID, Provider: "github", SourceKey: h.binding.SourceKey, RemoteID: h.binding.RemoteID, DisplayName: h.binding.DisplayName, Config: raw, IntervalSeconds: 300})
			require.NoError(t, err)
			rest, graphql, repositories := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/example-owner/example-repo" {
					repositories++
					if repositories == 2 {
						// End at content preparation so only the real status phase is counted.
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					rest++
					require.NoError(t, json.MarshalWrite(w, h.fetcher.repo))
					return
				}
				if r.URL.Path == "/graphql" {
					graphql++
					if unavailable {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					nodes := map[string]any{}
					for n := 1; n <= 100; n++ {
						state := "OPEN"
						if n == 1 {
							state = "CLOSED"
						}
						nodes[fmt.Sprintf("i%d", n-1)] = map[string]any{"fullDatabaseId": 1000 + n, "number": n, "state": state, "updatedAt": h.now, "closedAt": h.now, "repository": map[string]any{"databaseId": 101}}
					}
					require.NoError(t, json.MarshalWrite(w, map[string]any{"data": map[string]any{"repository": nodes}}))
					return
				}
				rest++
				rows := make([]map[string]any, 0, 100)
				for n := 1; n <= 100; n++ {
					row := statusIssueWire("open")
					row["id"], row["number"] = 1000+n, n
					row["url"] = fmt.Sprintf("https://api.github.com/repos/example-owner/example-repo/issues/%d", n)
					row["updated_at"] = h.now
					rows = append(rows, row)
				}
				if r.URL.Path == "/repos/example-owner/example-repo/issues" {
					require.NoError(t, json.MarshalWrite(w, rows))
					return
				}
				n, err := strconv.Atoi(r.URL.Path[len("/repos/example-owner/example-repo/issues/"):])
				require.NoError(t, err)
				if n == 1 {
					rows[0]["state"], rows[0]["closed_at"] = "closed", h.now
				}
				require.NoError(t, json.MarshalWrite(w, rows[n-1]))
			}))
			defer server.Close()
			h.runner.config.Store = h.db
			h.runner.config.Fetcher = NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL, GraphQLURLOverride: server.URL + "/graphql"})
			_, err = h.runner.RunOnce(h.ctx, b.ID)
			require.ErrorContains(t, err, "401")
			mapping, err := h.db.ImportMappingBySource(h.ctx, b.ProjectID, b.SourceKey, "issue", "issue-id:1001")
			require.NoError(t, err)
			issue, err := h.db.IssueByID(h.ctx, *mapping.IssueID)
			require.NoError(t, err)
			require.Equal(t, "closed", issue.Status)
			t.Logf("status phase: %d REST + %d GraphQL = %d requests", rest, graphql, rest+graphql)
			if unavailable {
				require.Equal(t, 102, rest)
			} else {
				require.Equal(t, 2, rest)
			}
			require.Equal(t, 1, graphql)
		})
	}
}

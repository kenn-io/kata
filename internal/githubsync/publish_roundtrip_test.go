package githubsync

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// Contract: publication and response-loss adoption hand ownership to ordinary import
// without allocating another local issue or changing the configured status mode.
func TestPublishRoundTrip(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "created", true: "response_lost"}[lost], func(t *testing.T) {
			creates := 0

			h := newRunnerHarness(t)
			store, b := h.db, h.binding
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/example-owner/example-repo":
					repo := publishGitHubRepo()
					repo["id"] = 101
					repo["node_id"] = b.RemoteID
					require.NoError(t, json.MarshalWrite(w, repo))
				case "/repos/example-owner/example-repo/issues":
					creates++
					if lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					}
					require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
				case "/repos/example-owner/example-repo/issues/7":
					require.NoError(t, json.MarshalWrite(w, statusIssueWire("open")))
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			fetcher := NewHTTPFetcher(HTTPFetcherConfig{Client: server.Client(), CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"), RESTBaseURLOverride: server.URL})
			adapter := NewPublishAdapter(fetcher)

			native, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: native.ID, Provider: "github", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			if lost {
				require.Error(t, err)
				_, err = publisher.Publish(t.Context(), request)
				require.Error(t, err)
				require.Equal(t, 1, creates)
				now = now.Add(time.Hour)
				request.Adopt = "7"
				_, err = publisher.Publish(t.Context(), request)
			}
			require.NoError(t, err)
			require.Equal(t, 1, creates)
			saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.JSONEq(t, string(b.Config), string(saved.Config))

			h.fetcher.issues = []Issue{testIssue(456, 7, "Upstream edit", now.Add(time.Minute))}
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: h.fetcher, Clock: func() time.Time { return now.Add(2 * time.Minute) }})

			result, err := runner.RunOnce(t.Context(), b.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "issue-id:456")
			require.NoError(t, err)
			require.Equal(t, native.ID, *mapping.IssueID)
			updated, err := store.IssueByID(t.Context(), native.ID)
			require.NoError(t, err)
			require.Equal(t, native.UID, updated.UID)
			require.Contains(t, updated.Title, "Upstream edit")

		})
	}
}

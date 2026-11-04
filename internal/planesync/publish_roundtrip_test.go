package planesync

import (
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

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case publishPlanePrefix():
					sendPlaneJSON(t, w, map[string]any{"id": testProjectID, "name": "Example project", "identifier": "EX"})
				case publishPlanePrefix() + "states/":
					sendPlaneJSON(t, w, wirePage(workflowRows(), false, ""))
				case publishPlanePrefix() + "work-items/":
					creates++
					if lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					}
					sendPlaneJSON(t, w, wireItem())
				case publishPlanePrefix() + "work-items/" + testItemID + "/":
					sendPlaneJSON(t, w, wireItem())
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			store := adapterStore(t)
			cfg := testConfig()
			cfg.APIOrigin = server.URL
			b := adapterBinding(t, store, cfg)
			adapter := NewAdapter(store, planeTestClient(server.URL))

			native, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: native.ID, Provider: "plane", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			if lost {
				require.Error(t, err)
				_, err = publisher.Publish(t.Context(), request)
				require.Error(t, err)
				require.Equal(t, 1, creates)
				now = now.Add(time.Hour)
				request.Adopt = testItemID
				_, err = publisher.Publish(t.Context(), request)
			}
			require.NoError(t, err)
			require.Equal(t, 1, creates)
			saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.JSONEq(t, string(b.Config), string(saved.Config))

			source := newAdapterSession()
			source.items[0].Name = "Upstream edit"
			source.items[0].UpdatedAt = now.Add(time.Minute)
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now.Add(2 * time.Minute) }})

			result, err := runner.RunOnce(t.Context(), b.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "work-item:"+testItemID)
			require.NoError(t, err)
			require.Equal(t, native.ID, *mapping.IssueID)
			updated, err := store.IssueByID(t.Context(), native.ID)
			require.NoError(t, err)
			require.Equal(t, native.UID, updated.UID)
			require.Contains(t, updated.Title, "Upstream edit")

		})
	}
}

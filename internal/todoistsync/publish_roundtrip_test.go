package todoistsync

import (
	"encoding/json/v2"
	"net/http"
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

			store, b, f := newRunnerFixture(t, "one-way")
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks" {
					creates++
					if lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					}
					require.NoError(t, json.MarshalWrite(w, taskJSON(f.row)))
					return
				}
				f.serve(w, r)
			})
			adapter := NewAdapter(store, f.client())

			native, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: native.ID, Provider: "todoist", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			if lost {
				require.Error(t, err)
				_, err = publisher.Publish(t.Context(), request)
				require.Error(t, err)
				require.Equal(t, 1, creates)
				now = now.Add(time.Hour)
				request.Adopt = f.row.ID
				_, err = publisher.Publish(t.Context(), request)
			}
			require.NoError(t, err)
			require.Equal(t, 1, creates)
			saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.JSONEq(t, string(b.Config), string(saved.Config))

			f.mu.Lock()
			f.now = now.Add(2 * time.Minute)
			f.row.Content = "Upstream edit"
			f.row.UpdatedAt = now.Add(time.Minute)
			f.mu.Unlock()
			runner := f.runner(store)

			result, err := runner.RunOnce(t.Context(), b.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:"+f.row.ID)
			require.NoError(t, err)
			require.Equal(t, native.ID, *mapping.IssueID)
			updated, err := store.IssueByID(t.Context(), native.ID)
			require.NoError(t, err)
			require.Equal(t, native.UID, updated.UID)
			require.Contains(t, updated.Title, "Upstream edit")

		})
	}
}

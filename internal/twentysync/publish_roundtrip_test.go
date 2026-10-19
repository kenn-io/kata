package twentysync

import (
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

			client, cfg, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/metadata" {
					fields := defaultFields()
					for _, f := range fields {
						f["isSystem"] = false
						f["isNullable"] = true
						f["defaultValue"] = nil
					}
					standardMetadata(t, w, r, workspaceID, fields)
					return
				}
				if r.Method == http.MethodPost {
					creates++
					if lost {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					}
					writeJSON(t, w, map[string]any{"data": map[string]any{"createTask": map[string]any{"id": taskID}}})
					return
				}
				require.Equal(t, "/rest/tasks/"+taskID, r.URL.Path)
				writeJSON(t, w, map[string]any{"data": map[string]any{"task": taskWire(taskID)}})
			})
			store := adapterStore(t)
			b := adapterBinding(t, store, cfg)
			adapter := NewAdapter(store, client)

			native, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: native.ID, Provider: "twenty", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			if lost {
				require.Error(t, err)
				_, err = publisher.Publish(t.Context(), request)
				require.Error(t, err)
				require.Equal(t, 1, creates)
				now = now.Add(time.Hour)
				request.Adopt = taskID
				_, err = publisher.Publish(t.Context(), request)
			}
			require.NoError(t, err)
			require.Equal(t, 1, creates)
			saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.JSONEq(t, string(b.Config), string(saved.Config))

			source := newAdapterSource(now)
			source.items[0].Title = "Upstream edit"
			source.items[0].UpdatedAt = now.Add(time.Minute)
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now.Add(2 * time.Minute) }})

			result, err := runner.RunOnce(t.Context(), b.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:"+taskID)
			require.NoError(t, err)
			require.Equal(t, native.ID, *mapping.IssueID)
			updated, err := store.IssueByID(t.Context(), native.ID)
			require.NoError(t, err)
			require.Equal(t, native.UID, updated.UID)
			require.Contains(t, updated.Title, "Upstream edit")

		})
	}
}

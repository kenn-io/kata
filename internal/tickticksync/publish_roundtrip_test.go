package tickticksync

import (
	"encoding/json/v2"
	"errors"
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

			store, b := adapterDB(t)
			task := Task{ID: "task-1", ProjectID: "project-1", Title: "Native", Content: "Body", Status: new(0), Kind: "TEXT"}
			client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/project/project-1" {
					return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
				}
				if r.Method == http.MethodPost {
					require.Equal(t, "/open/v1/task", r.URL.Path)
					creates++
					if lost {
						return nil, errors.New("response lost")
					}
				} else {
					require.Equal(t, "/open/v1/project/project-1/task/task-1", r.URL.Path)
				}
				raw, err := json.Marshal(task)
				require.NoError(t, err)
				return reply(200, string(raw)), nil
			})
			adapter := NewAdapter(store, client)

			native, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
			require.NoError(t, err)
			now := time.Now().UTC()
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: native.ID, Provider: "ticktick", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			if lost {
				require.Error(t, err)
				_, err = publisher.Publish(t.Context(), request)
				require.Error(t, err)
				require.Equal(t, 1, creates)
				now = now.Add(time.Hour)
				request.Adopt = "task-1"
				_, err = publisher.Publish(t.Context(), request)
			}
			require.NoError(t, err)
			require.Equal(t, 1, creates)
			saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
			require.NoError(t, err)
			require.JSONEq(t, string(b.Config), string(saved.Config))

			source := sourceData()
			task.Title = "Upstream edit"
			source.data.Tasks = []Task{task}
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now.Add(2 * time.Minute) }})

			result, err := runner.RunOnce(t.Context(), b.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:task-1")
			require.NoError(t, err)
			require.Equal(t, native.ID, *mapping.IssueID)
			updated, err := store.IssueByID(t.Context(), native.ID)
			require.NoError(t, err)
			require.Equal(t, native.UID, updated.UID)
			require.Contains(t, updated.Title, "Upstream edit")

		})
	}
}

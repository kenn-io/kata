package tickticksync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

func openPublishTickTick(t *testing.T, c *Client) issuesync.PublishSession {
	t.Helper()
	cfg := Config{ProjectID: "project-1"}
	raw, err := EncodeConfig(cfg)
	require.NoError(t, err)
	s, err := NewAdapter(nil, c).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "ticktick", SourceKey: cfg.SourceKey(), RemoteID: cfg.RemoteID(), Config: raw})
	require.NoError(t, err)
	return s
}
func TestPublishTickTickContentAndRecovery(t *testing.T) {
	for _, body := range []string{"", "**世界**\nBody"} {
		t.Run(body, func(t *testing.T) {
			creates, admissions := 0, 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				require.Equal(t, "api.ticktick.com", r.URL.Host)
				require.Equal(t, "Bearer example-secret", r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/open/v1/project/project-1":
					return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
				case "/open/v1/task":
					require.Equal(t, http.MethodPost, r.Method)
					creates++
					require.Equal(t, 1, admissions)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					require.Equal(t, map[string]any{"title": "Native 🐟", "projectId": "project-1", "content": body}, payload)
					return reply(200, `{"id":"task-1","projectId":"project-1","status":0,"kind":"TEXT"}`), nil
				case "/open/v1/project/project-1/task/task-1":
					return reply(200, `{"id":"task-1","projectId":"project-1","status":0,"kind":"TEXT"}`), nil
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					return reply(404, `{}`), nil
				}
			})
			s := openPublishTickTick(t, c)
			payload, err := s.PrepareCreate(t.Context(), db.Issue{Title: "Native 🐟", Body: body, Status: "open"})
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error { admissions++; return nil })
			require.NoError(t, err)
			require.Equal(t, "task:task-1", remote.ExternalID)
			require.Equal(t, "open", remote.Status)
			require.Nil(t, remote.SourceUpdatedAt)
			require.False(t, remote.Version.IsZero())
			_, err = s.ReadCreated(t.Context(), remote)
			require.NoError(t, err)
			_, err = s.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: "task-1"})
			require.NoError(t, err)
			require.Equal(t, 1, creates)
		})
	}
}
func TestPublishTickTickOneSendAndProjectPolicy(t *testing.T) {
	for _, scenario := range []string{"NOTE", "closed_project", "read", "comment", "wrong_project", "drop", "503", "empty", "malformed", "redirect", "closed_task", "incomplete", "admission", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			creates := 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/project/project-1" {
					project := Project{ID: "project-1", Kind: "TASK", Permission: "write"}
					switch scenario {
					case "NOTE":
						project.Kind = "NOTE"
					case "closed_project":
						project.Closed = true
					case "read", "comment":
						project.Permission = scenario
					}
					raw, err := json.Marshal(project)
					require.NoError(t, err)
					return reply(200, string(raw)), nil
				}
				require.Equal(t, "/open/v1/task", r.URL.Path)
				creates++
				switch scenario {
				case "drop":
					return nil, errors.New("lost response")
				case "503":
					return reply(503, "secret body"), nil
				case "empty":
					return reply(201, ""), nil
				case "malformed":
					return reply(200, `{"id":`), nil
				case "redirect":
					res := reply(307, "")
					res.Header.Set("Location", "https://untrusted.example")
					return res, nil
				}
				task := Task{ID: "task-1", ProjectID: "project-1", Status: new(0), Kind: "TEXT"}
				switch scenario {
				case "wrong_project":
					task.ProjectID = "other-project"
				case "closed_task":
					task.Status = new(2)
				case "incomplete":
					task.Status = nil
				}
				raw, err := json.Marshal(task)
				require.NoError(t, err)
				return reply(200, string(raw)), nil
			})
			s := openPublishTickTick(t, c)
			issue := db.Issue{Title: "Native", Status: "open"}
			if scenario == "oversize" {
				issue.Body = strings.Repeat("x", (1<<20)+1)
			}
			payload, err := s.PrepareCreate(t.Context(), issue)
			if scenario == "NOTE" || scenario == "closed_project" || scenario == "read" || scenario == "comment" || scenario == "oversize" {
				require.Error(t, err)
				require.Zero(t, creates)
				return
			}
			require.NoError(t, err)
			remote, err := s.Create(t.Context(), payload, func(context.Context) error {
				if scenario == "admission" {
					return errors.New("denied")
				}
				return nil
			})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
			if scenario == "admission" {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
			if scenario == "incomplete" || scenario == "closed_task" {
				require.Equal(t, "task:task-1", remote.ExternalID)
			}
		})
	}
}
func TestPublishTickTickPreservesCheckpointAndImportsSameIssue(t *testing.T) {
	store, b := adapterDB(t)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
	require.NoError(t, err)
	task := Task{ID: "task-1", ProjectID: "project-1", Title: "Native", Content: "Body", Status: new(0), Kind: "TEXT"}
	client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/project/project-1" {
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		}
		require.Equal(t, "/open/v1/task", r.URL.Path)
		raw, err := json.Marshal(task)
		require.NoError(t, err)
		return reply(200, string(raw)), nil
	})
	_, err = issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: NewAdapter(store, client)}).Publish(t.Context(), issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker"})
	require.NoError(t, err)
	saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
	require.NoError(t, err)
	require.JSONEq(t, string(b.Config), string(saved.Config))
	mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:task-1")
	require.NoError(t, err)
	require.Nil(t, mapping.SourceUpdatedAt)
	source := sourceData()
	source.data.Tasks = []Task{task}
	now := time.Now().UTC().Add(time.Minute)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now }})
	result, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, result.Import.Created)
	require.Equal(t, issue.UID, mappedIssue(t, store, b, "task-1").UID)
	checkpoint, err := DecodeCheckpoint(result.Binding.Config)
	require.NoError(t, err)
	require.NotEmpty(t, checkpoint.Versions["task-1"].Hash)
	version := checkpoint.Versions["task-1"].Version
	now = now.Add(time.Minute)
	result, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	checkpoint, err = DecodeCheckpoint(result.Binding.Config)
	require.NoError(t, err)
	require.Equal(t, version, checkpoint.Versions["task-1"].Version)
}

func TestPublishTickTickRecoversCompletionBeforeFirstImport(t *testing.T) {
	store, b := adapterDB(t)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
	require.NoError(t, err)
	task := Task{ID: "task-1", ProjectID: "project-1", Title: "Native", Content: "Body", Status: new(0), Kind: "TEXT"}
	client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/project/project-1" {
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		}
		require.Equal(t, "/open/v1/task", r.URL.Path)
		raw, err := json.Marshal(task)
		require.NoError(t, err)
		return reply(200, string(raw)), nil
	})
	_, err = issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: NewAdapter(store, client)}).Publish(t.Context(), issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker"})
	require.NoError(t, err)
	mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:task-1")
	require.NoError(t, err)
	require.Nil(t, mapping.SourceUpdatedAt, "publication must not invent a source content timestamp")
	_, _, _, err = store.CloseIssueWithEvents(t.Context(), issue.ID, "done", "worker", "", nil)
	require.NoError(t, err)

	task.Title, task.Content, task.Status = "Finished remote task", "Final remote content", new(2)
	task.CompletedTime = "2026-10-01T09:00:00.000+0000"
	source := sourceData()
	source.data.Tasks = nil // TickTick's collection excludes completed tasks.
	source.missing[task.ID] = task
	now := time.Now().UTC().Add(time.Minute)
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now }})
	result, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, result.Import.Created)
	completed := mappedIssue(t, store, b, task.ID)
	require.Equal(t, task.Title, completed.Title)
	require.Contains(t, completed.Body, task.Content)
	require.Equal(t, issue.UID, completed.UID)
	require.Equal(t, []string{task.ID}, source.reads, "the first import must read a completed publication even when the native issue is closed")
	mapping, err = store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:task-1")
	require.NoError(t, err)
	require.NotNil(t, mapping.SourceUpdatedAt)
	require.True(t, mapping.SourceUpdatedAt.Equal(now.Truncate(time.Millisecond)), "content is dated by the ordinary recovery observation")

	source.reads = nil
	now = now.Add(time.Minute)
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Empty(t, source.reads, "a recorded completion must leave the bounded recovery window")
	task.Status, task.CompletedTime = new(0), ""
	source.data.Tasks = []Task{task}
	now = now.Add(time.Minute)
	result, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, result.Import.Created)
	reopened := mappedIssue(t, store, b, task.ID)
	require.Equal(t, issue.UID, reopened.UID)
	require.Equal(t, "open", reopened.Status, "TickTick's reopen must follow the recorded completion")
}

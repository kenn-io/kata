package tickticksync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
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
	for _, scenario := range []string{"NOTE", "comment", "wrong_project", "drop", "503", "empty", "malformed", "redirect", "closed_task", "incomplete", "admission"} {
		t.Run(scenario, func(t *testing.T) {
			creates := 0
			c, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/project/project-1" {
					project := Project{ID: "project-1", Kind: "TASK", Permission: "write"}
					switch scenario {
					case "NOTE":
						project.Kind = "NOTE"
					case "comment":
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
			payload, err := s.PrepareCreate(t.Context(), issue)
			if scenario == "NOTE" || scenario == "comment" {
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
			if scenario == "incomplete" {
				require.NotNil(t, remote.ContentBaseline, "a partial create observation must retain its prepared content baseline")
				require.True(t, validPublicationContentBaseline(*remote.ContentBaseline))
			}
		})
	}
}
func TestPublishTickTickPreservesCheckpointAndImportsSameIssue(t *testing.T) {
	store, b := adapterDB(t)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: b.ProjectID, Title: "Native", Body: "Body", Author: "worker"})
	require.NoError(t, err)
	// TickTick may omit kind on a text-task response. The publication baseline
	// uses the prepared TEXT task, so this unchanged first import must compare
	// equal and preserve local edits made after publication.
	task := Task{ID: "task-1", ProjectID: "project-1", Title: "Native", Content: "Body", Status: new(0), Items: []ChecklistItem{}}
	taskResponseWithoutKind := func() string {
		raw, err := json.Marshal(map[string]any{
			"id": task.ID, "projectId": task.ProjectID, "title": task.Title, "content": task.Content,
			"status": *task.Status, "items": task.Items,
		})
		require.NoError(t, err)
		return string(raw)
	}
	client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/project/project-1":
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		case "/open/v1/task", "/open/v1/project/project-1/task/task-1":
			return reply(200, taskResponseWithoutKind()), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return reply(404, `{}`), nil
		}
	})
	_, err = issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: NewAdapter(store, client)}).Publish(t.Context(), issuesync.PublishRequest{ProjectID: b.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker"})
	require.NoError(t, err)
	saved, err := store.IssueSyncBindingByProject(t.Context(), b.ProjectID)
	require.NoError(t, err)
	publicConfig, err := db.PublicIssueSyncConfig(saved.Config)
	require.NoError(t, err)
	require.JSONEq(t, string(b.Config), string(publicConfig))
	var persisted struct {
		ProviderCheckpoint struct {
			PublicationContentBaselines map[string]struct {
				Hash string    `json:"hash"`
				At   time.Time `json:"at"`
			} `json:"publication_content_baselines"`
		} `json:"_provider_checkpoint"`
	}
	require.NoError(t, json.Unmarshal(saved.Config, &persisted))
	preparedTask := task
	preparedTask.Kind = "TEXT"
	publishedHash, err := publicationContentHash(preparedTask)
	require.NoError(t, err)
	baseline := persisted.ProviderCheckpoint.PublicationContentBaselines["task:task-1"]
	require.Equal(t, publishedHash, baseline.Hash)
	require.True(t, baseline.At.Equal(issue.UpdatedAt.UTC().Truncate(time.Millisecond)))
	mapping, err := store.ImportMappingBySource(t.Context(), b.ProjectID, b.SourceKey, "issue", "task:task-1")
	require.NoError(t, err)
	require.Nil(t, mapping.SourceUpdatedAt)
	localTitle, localBody := "Local title after publication", "Local body after publication"
	local, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{
		IssueID: issue.ID, Title: &localTitle, Body: &localBody, Actor: "worker",
	})
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "target-project")
	require.NoError(t, err)
	_, err = store.UpsertIssueSyncBinding(t.Context(), db.UpsertIssueSyncBindingParams{
		ProjectID: target.ID, Provider: "ticktick", SourceKey: b.SourceKey, RemoteID: b.RemoteID,
		DisplayName: b.DisplayName, Config: b.Config, IntervalSeconds: 300,
	})
	require.NoError(t, err)
	source := sourceData()
	source.data.Tasks = []Task{task}
	now := time.Now().UTC().Add(time.Minute)
	wrapped := &failOnceImportStore{Store: store, failNext: true}
	runner := NewRunner(RunnerConfig{Store: wrapped, Fetcher: source, Clock: func() time.Time { return now }})
	_, err = runner.RunOnce(t.Context(), b.ID)
	require.ErrorContains(t, err, "injected final content import failure")
	stagedBinding, err := store.IssueSyncBindingByID(t.Context(), b.ID)
	require.NoError(t, err)
	stagedCheckpoint, err := DecodeCheckpoint(stagedBinding.Config)
	require.NoError(t, err)
	require.Contains(t, stagedCheckpoint.PublicationContentBaselines, "task:task-1", "a failed first import must retain its publication baseline")
	for _, dryRun := range []bool{true, false} {
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: b.ProjectID, ToProjectID: target.ID,
			IfMatchRev: local.Revision, Actor: "worker", DryRun: dryRun,
		})
		require.ErrorIs(t, err, db.ErrImportValidation, "an unconsumed TickTick publication baseline must fence a move")
	}
	stillLocal := mappedIssue(t, store, b, "task-1")
	require.Equal(t, local.Title, stillLocal.Title)
	require.Equal(t, local.Body, stillLocal.Body)
	result, err := runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	require.Zero(t, result.Import.Created)
	afterFirstPoll := mappedIssue(t, store, b, "task-1")
	require.Equal(t, issue.UID, afterFirstPoll.UID)
	require.Equal(t, local.Title, afterFirstPoll.Title, "an unchanged first poll must preserve local title edits")
	require.Equal(t, local.Body, afterFirstPoll.Body, "an unchanged first poll must preserve local body edits")
	checkpoint, err := DecodeCheckpoint(result.Binding.Config)
	require.NoError(t, err)
	require.NotEmpty(t, checkpoint.Versions["task-1"].Hash)
	var consumed struct {
		ProviderCheckpoint struct {
			PublicationContentBaselines map[string]any `json:"publication_content_baselines"`
		} `json:"_provider_checkpoint"`
	}
	require.NoError(t, json.Unmarshal(result.Binding.Config, &consumed))
	require.Empty(t, consumed.ProviderCheckpoint.PublicationContentBaselines)
	version := checkpoint.Versions["task-1"].Version
	now = now.Add(time.Minute)
	result, err = runner.RunOnce(t.Context(), b.ID)
	require.NoError(t, err)
	checkpoint, err = DecodeCheckpoint(result.Binding.Config)
	require.NoError(t, err)
	require.Equal(t, version, checkpoint.Versions["task-1"].Version)
}

func TestPublishTickTickResponseLossPreservesOriginalContentBaseline(t *testing.T) {
	for _, stagedBeforeRecovery := range []bool{false, true} {
		name := "direct_recovery"
		if stagedBeforeRecovery {
			name = "failed_scheduled_sync_before_recovery"
		}
		t.Run(name, func(t *testing.T) {
			store, binding := adapterDB(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: binding.ProjectID, Title: "Native", Body: "Published body", Author: "worker",
			})
			require.NoError(t, err)

			created := Task{ID: "task-1", ProjectID: "project-1", Title: issue.Title, Content: issue.Body, Status: new(0), Kind: "TEXT", Items: []ChecklistItem{}}
			createRequests := 0
			client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/open/v1/project/project-1":
					return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
				case "/open/v1/task":
					createRequests++
					return nil, errors.New("response lost after creation")
				case "/open/v1/project/project-1/task/task-1":
					raw, marshalErr := json.Marshal(created)
					require.NoError(t, marshalErr)
					return reply(200, string(raw)), nil
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					return reply(404, `{}`), nil
				}
			})
			publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: NewAdapter(store, client), Clock: func() time.Time { return now }})
			request := issuesync.PublishRequest{ProjectID: binding.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker"}
			_, err = publisher.Publish(t.Context(), request)
			require.Error(t, err)
			require.Equal(t, 1, createRequests, "the uncertain create must not be sent again")

			localTitle, localBody := "Local title after publication", "Local body after publication"
			local, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{
				IssueID: issue.ID, Title: &localTitle, Body: &localBody, Actor: "worker",
			})
			require.NoError(t, err)

			// The pending publication owns its claim until the normal recovery TTL
			// expires. A later scheduled sync is still allowed to stage its checkpoint.
			now = now.Add(31 * time.Minute)
			source := sourceData()
			source.data.Tasks = []Task{created}
			runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now }})
			if stagedBeforeRecovery {
				_, err = runner.RunOnce(t.Context(), binding.ID)
				require.Error(t, err, "the pending publication fences the scheduled import")
				stagedBinding, readErr := store.IssueSyncBindingByProject(t.Context(), binding.ProjectID)
				require.NoError(t, readErr)
				staged, decodeErr := DecodeCheckpoint(stagedBinding.Config)
				require.NoError(t, decodeErr)
				require.Contains(t, staged.Versions, "task-1", "the failed run persisted its pre-import source checkpoint")
			}

			_, err = publisher.Publish(t.Context(), issuesync.PublishRequest{
				ProjectID: binding.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker", Adopt: "task-1",
			})
			require.NoError(t, err)
			attached, err := store.IssueSyncBindingByProject(t.Context(), binding.ProjectID)
			require.NoError(t, err)
			attachedCheckpoint, err := DecodeCheckpoint(attached.Config)
			require.NoError(t, err)
			baseline := attachedCheckpoint.PublicationContentBaselines["task:task-1"]
			require.Equal(t, issue.UpdatedAt.UTC().Truncate(time.Millisecond), baseline.At,
				"recovery must keep the content snapshot time from before dispatch")

			now = now.Add(10 * time.Minute)
			result, err := runner.RunOnce(t.Context(), binding.ID)
			require.NoError(t, err)
			require.Zero(t, result.Import.Created)
			after := mappedIssue(t, store, binding, "task-1")
			require.Equal(t, issue.UID, after.UID)
			require.Equal(t, local.Title, after.Title, "an unchanged first poll must preserve a local title edit")
			require.Equal(t, local.Body, after.Body, "an unchanged first poll must preserve a local body edit")
			checkpoint, err := DecodeCheckpoint(result.Binding.Config)
			require.NoError(t, err)
			require.Equal(t, issue.UpdatedAt.UTC().Truncate(time.Millisecond), checkpoint.Versions["task-1"].Version)
			require.Equal(t, 1, createRequests)
		})
	}
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

func TestPublishTickTickIncompleteCreateObservationPreservesLocalEdits(t *testing.T) {
	store, binding := adapterDB(t)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: binding.ProjectID, Title: "Native", Body: "Published body", Author: "worker",
	})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	created := Task{ID: "task-1", ProjectID: "project-1", Title: issue.Title, Content: issue.Body, Status: new(0), Items: []ChecklistItem{}}
	partial := Task{ID: "task-1", ProjectID: "project-1", Kind: "TEXT"}
	createRequests := 0
	client, _ := testClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/project/project-1":
			return reply(200, `{"id":"project-1","kind":"TASK","permission":"write"}`), nil
		case "/open/v1/task":
			createRequests++
			raw, marshalErr := json.Marshal(partial)
			require.NoError(t, marshalErr)
			return reply(200, string(raw)), nil
		case "/open/v1/project/project-1/task/task-1":
			raw, marshalErr := json.Marshal(created)
			require.NoError(t, marshalErr)
			return reply(200, string(raw)), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return reply(404, `{}`), nil
		}
	})
	adapter := NewAdapter(store, client)
	publisher := issuesync.NewPublisher(issuesync.PublisherConfig{Store: store, Adapter: adapter, Clock: func() time.Time { return now }})
	request := issuesync.PublishRequest{ProjectID: binding.ProjectID, IssueID: issue.ID, Provider: "ticktick", Actor: "worker"}
	_, err = publisher.Publish(t.Context(), request)
	require.Error(t, err, "the incomplete create observation must retain its confirmed identity for recovery")
	require.Equal(t, 1, createRequests)
	pending, err := store.PendingIssuePublication(t.Context(), binding.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.NotNil(t, pending.Remote)
	require.NotNil(t, pending.Remote.ContentBaseline, "recovery metadata must survive request compaction")
	require.Equal(t, issue.UpdatedAt.UTC().Truncate(time.Millisecond), pending.Remote.ContentBaseline.At)

	localTitle, localBody := "Local title after publication", "Local body after publication"
	local, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &localTitle, Body: &localBody, Actor: "worker"})
	require.NoError(t, err)
	now = now.Add(31 * time.Minute)
	result, err := publisher.Publish(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "recovered", result.Result)
	require.Equal(t, 1, createRequests, "recovery must not send a second create")

	now = now.Add(time.Minute)
	source := sourceData()
	source.data.Tasks = []Task{created}
	runner := NewRunner(RunnerConfig{Store: store, Fetcher: source, Clock: func() time.Time { return now }})
	imported, err := runner.RunOnce(t.Context(), binding.ID)
	require.NoError(t, err)
	require.Zero(t, imported.Import.Created)
	after := mappedIssue(t, store, binding, "task-1")
	require.Equal(t, local.Title, after.Title, "an unchanged first poll must preserve a local title edit")
	require.Equal(t, local.Body, after.Body, "an unchanged first poll must preserve a local body edit")
	checkpoint, err := DecodeCheckpoint(imported.Binding.Config)
	require.NoError(t, err)
	require.Equal(t, issue.UpdatedAt.UTC().Truncate(time.Millisecond), checkpoint.Versions["task-1"].Version)
}

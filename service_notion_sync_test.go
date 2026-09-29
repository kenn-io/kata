package kata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/notionsync"
)

func TestServiceNotionScheduledProgress(t *testing.T) {
	ctx := context.Background()
	fetcher := newRuntimeNotionFetcher()
	var resolved config.NotionSyncConfig
	service, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, NotionSync: NotionSyncConfig{TokenEnv: " EXAMPLE_NOTION_TOKEN "}}, serviceDeps{notionSyncFetcherFactory: func(cfg config.NotionSyncConfig) notionsync.Fetcher { resolved = cfg; return fetcher }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	require.Equal(t, "EXAMPLE_NOTION_TOKEN", resolved.TokenEnv)
	// Enable and scheduled polling must share the configured fetcher.
	close(fetcher.releaseSource)
	project, err := service.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	enable := httptest.NewRecorder()
	service.Handler().ServeHTTP(enable, newRuntimeNotionPost(fmt.Sprintf("/api/v1/projects/%d/issue-sync/notion/enable", project.ID), strings.NewReader(`{"config":{"data_source_id":"11111111-1111-1111-1111-111111111111","done_statuses":["Delivered"]}}`)))
	require.Equal(t, http.StatusOK, enable.Code, enable.Body.String())
	binding, err := service.store.IssueSyncBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, 300, binding.IntervalSeconds)
	sub := service.broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	recording := &runtimeNotionRecordingStore{Storage: service.store, recorded: make(chan db.IssueSyncStatus, 1)}
	service.store = recording
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.Fail(t, "Run did not stop")
		}
	})
	waitRuntimeNotion(t, fetcher.contentStarted)
	status := readRuntimeNotionStatus(t, service.Handler(), project.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "content", status.Status.Progress.Phase)
	require.Zero(t, status.Status.Progress.Completed)
	require.Equal(t, 1, status.Status.Progress.Total)
	durable, err := service.store.IssueSyncStatusByProject(ctx, project.ID)
	require.NoError(t, err)
	require.NotNil(t, durable.SyncStartedAt)
	require.Equal(t, *durable.SyncStartedAt, status.Status.Progress.StartedAt)
	require.Equal(t, "content", service.notionSyncProgress.Snapshot(binding.ID, *durable.SyncStartedAt).Phase)
	once := httptest.NewRecorder()
	service.Handler().ServeHTTP(once, newRuntimeNotionPost(fmt.Sprintf("/api/v1/projects/%d/issue-sync/notion/once", project.ID), strings.NewReader(`{}`)))
	require.Equal(t, http.StatusConflict, once.Code, once.Body.String())
	require.Equal(t, status.Status.Progress, readRuntimeNotionStatus(t, service.Handler(), project.ID).Status.Progress)
	close(fetcher.releaseContent)
	committed := waitRuntimeNotionRecord(t, recording.recorded)
	require.NotNil(t, committed.LastSuccessAt)
	status = readRuntimeNotionStatus(t, service.Handler(), project.ID)
	require.Nil(t, status.Status.Progress)
	require.Equal(t, 1, status.Status.LastCreated)
	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "missing committed issue event")
	}
}
func TestServiceNotionInitialSourceAndGitHubRepositoryProgress(t *testing.T) {
	ctx := context.Background()
	fetcher := newRuntimeNotionFetcher()
	github := &runtimeGitHubSourceFetcher{started: make(chan struct{})}
	service, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}}, serviceDeps{notionSyncFetcher: fetcher, gitHubSyncFetcher: github})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	p, err := service.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	binding := runtimeNotionBinding(t, service.store, p.ID)
	g, err := service.store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	raw, err := githubsync.EncodeConfig(githubsync.Config{Host: "github.com", Owner: "example-owner", Repo: "example-repo", RepoID: 12345})
	require.NoError(t, err)
	_, err = service.store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: g.ID, Provider: "github", SourceKey: "github:R_exampleNode", RemoteID: "R_exampleNode", DisplayName: "example-owner/example-repo", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	waitRuntimeNotion(t, fetcher.sourceStarted)
	waitRuntimeNotion(t, github.started)
	status := readRuntimeNotionStatus(t, service.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "source", status.Status.Progress.Phase)
	durable, err := service.store.IssueSyncStatusByProject(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "source", service.notionSyncProgress.Snapshot(binding.ID, *durable.SyncStartedAt).Phase)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/github/status", g.ID), nil))
	require.Equal(t, 200, response.Code)
	var out runtimeNotionStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	require.NotNil(t, out.Status.Progress)
	require.Equal(t, "repository", out.Status.Progress.Phase)
}

type runtimeGitHubSourceFetcher struct {
	serviceGitHubFetcher
	started chan struct{}
}

func (f *runtimeGitHubSourceFetcher) Repository(ctx context.Context, _, _, _ string) (githubsync.Repository, error) {
	close(f.started)
	<-ctx.Done()
	return githubsync.Repository{}, ctx.Err()
}
func TestServiceNotionConfigRejectsInvalidSelector(t *testing.T) {
	_, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, NotionSync: NotionSyncConfig{TokenEnv: "INVALID=NAME"}})
	require.ErrorContains(t, err, "notion_sync.token_env")
}

const runtimeNotionSourceID = "11111111-1111-1111-1111-111111111111"
const runtimeNotionDatabaseID = "22222222-2222-2222-2222-222222222222"

type runtimeNotionFetcher struct {
	sourceStarted, releaseSource, contentStarted, releaseContent chan struct{}
	sourceOnce, contentOnce                                      sync.Once
}

func newRuntimeNotionFetcher() *runtimeNotionFetcher {
	return &runtimeNotionFetcher{sourceStarted: make(chan struct{}), releaseSource: make(chan struct{}), contentStarted: make(chan struct{}), releaseContent: make(chan struct{})}
}
func (f *runtimeNotionFetcher) ForRun(context.Context) (notionsync.Session, error) { return f, nil }
func (f *runtimeNotionFetcher) DataSource(ctx context.Context, id string) (notionsync.DataSource, error) {
	f.sourceOnce.Do(func() { close(f.sourceStarted) })
	select {
	case <-f.releaseSource:
	case <-ctx.Done():
		return notionsync.DataSource{}, ctx.Err()
	}
	return notionsync.DataSource{ID: id, DatabaseID: runtimeNotionDatabaseID, Name: "Tasks", Properties: []notionsync.Property{
		{ID: "title", Name: "Task", Type: "title"}, {ID: "status", Name: "State", Type: "status", Options: []notionsync.Option{{ID: "done", Name: "Delivered"}}}, {ID: "people", Name: "Owner", Type: "people"},
	}}, nil
}
func (*runtimeNotionFetcher) Database(context.Context, string) (notionsync.Database, error) {
	return notionsync.Database{ID: runtimeNotionDatabaseID, DataSources: []notionsync.Option{{ID: runtimeNotionSourceID, Name: "Tasks"}}}, nil
}
func (*runtimeNotionFetcher) Pages(context.Context, notionsync.Config, *time.Time) ([]notionsync.Page, error) {
	return []notionsync.Page{{ID: "33333333-3333-3333-3333-333333333333", URL: "https://www.notion.so/33333333333333333333333333333333", DataSourceID: runtimeNotionSourceID, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}}, nil
}
func (f *runtimeNotionFetcher) Content(ctx context.Context, _ notionsync.Config, p notionsync.Page) (notionsync.PageContent, error) {
	f.contentOnce.Do(func() { close(f.contentStarted) })
	select {
	case <-f.releaseContent:
	case <-ctx.Done():
		return notionsync.PageContent{}, ctx.Err()
	}
	return notionsync.PageContent{Page: p, Title: "Scheduled task", Markdown: "Body"}, nil
}
func runtimeNotionBinding(t *testing.T, store db.Storage, projectID int64) db.IssueSyncBinding {
	t.Helper()
	raw, err := notionsync.EncodeConfig(notionsync.Config{DataSourceID: runtimeNotionSourceID, DatabaseID: runtimeNotionDatabaseID, TitlePropertyID: "title", StatusPropertyID: "status", AssigneePropertyID: "people", DoneStatusIDs: []string{"done"}})
	require.NoError(t, err)
	binding, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: projectID, Provider: "notion", SourceKey: "notion:" + runtimeNotionSourceID, RemoteID: runtimeNotionSourceID, DisplayName: "Tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return binding
}

type runtimeNotionStatus struct {
	Status struct {
		State       string `json:"state"`
		LastCreated int    `json:"last_created"`
		Progress    *struct {
			Phase            string `json:"phase"`
			Completed, Total int
			StartedAt        time.Time `json:"started_at"`
		} `json:"progress"`
	} `json:"status"`
}

func readRuntimeNotionStatus(t *testing.T, handler http.Handler, projectID int64) runtimeNotionStatus {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/notion/status", projectID), nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var status runtimeNotionStatus
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &status))
	return status
}
func waitRuntimeNotion(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "scheduled Notion worker did not reach upstream read")
	}
}

func newRuntimeNotionPost(path string, body io.Reader) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, body)
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestServiceNotionExplicitCredentialDoesNotFallBack(t *testing.T) {
	t.Setenv("KATA_NOTION_TOKEN", "default-example-token")
	t.Setenv("EXAMPLE_NOTION_TOKEN", "")
	service, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, NotionSync: NotionSyncConfig{TokenEnv: " EXAMPLE_NOTION_TOKEN "}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	enable := httptest.NewRecorder()
	service.Handler().ServeHTTP(enable, newRuntimeNotionPost(fmt.Sprintf("/api/v1/projects/%d/issue-sync/notion/enable", project.ID), strings.NewReader(`{"config":{"data_source_id":"11111111-1111-1111-1111-111111111111","done_statuses":["Delivered"]}}`)))
	require.Equal(t, http.StatusBadRequest, enable.Code, enable.Body.String())
	require.Contains(t, enable.Body.String(), "credentials")
	runtimeNotionBinding(t, service.store, project.ID)
	recording := &runtimeNotionRecordingStore{Storage: service.store, recorded: make(chan db.IssueSyncStatus, 1)}
	service.store = recording
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	status := waitRuntimeNotionRecord(t, recording.recorded)
	require.NotNil(t, status.LastErrorAt)
	require.Nil(t, status.LastSuccessAt)
}

// runtimeNotionRecordingStore exposes the real durable completion to tests.
// It delegates all product storage behavior before signaling the observer.
type runtimeNotionRecordingStore struct {
	db.Storage
	recorded chan db.IssueSyncStatus
}

func (s *runtimeNotionRecordingStore) RecordIssueSyncSuccess(ctx context.Context, params db.IssueSyncSuccessParams) (db.IssueSyncStatus, error) {
	status, err := s.Storage.RecordIssueSyncSuccess(ctx, params)
	if err == nil {
		s.recorded <- status
	}
	return status, err
}
func (s *runtimeNotionRecordingStore) RecordIssueSyncError(ctx context.Context, params db.IssueSyncErrorParams) (db.IssueSyncStatus, error) {
	status, err := s.Storage.RecordIssueSyncError(ctx, params)
	if err == nil {
		s.recorded <- status
	}
	return status, err
}
func waitRuntimeNotionRecord(t *testing.T, recorded <-chan db.IssueSyncStatus) db.IssueSyncStatus {
	t.Helper()
	select {
	case status := <-recorded:
		return status
	case <-time.After(5 * time.Second):
		require.FailNow(t, "scheduled Notion worker did not record durable completion")
	}
	return db.IssueSyncStatus{}
}

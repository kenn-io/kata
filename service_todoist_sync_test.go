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
	"go.kenn.io/kata/internal/todoistsync"
)

type runtimeTodoistFetcher struct {
	started, release chan struct{}
	once             sync.Once
}

func newRuntimeTodoistFetcher() *runtimeTodoistFetcher {
	return &runtimeTodoistFetcher{started: make(chan struct{}), release: make(chan struct{})}
}
func (f *runtimeTodoistFetcher) Account(context.Context) (string, error) { return "1234567", nil }
func (f *runtimeTodoistFetcher) ForRun(context.Context, todoistsync.Config) (todoistsync.Session, error) {
	return f, nil
}
func (f *runtimeTodoistFetcher) Project(ctx context.Context, c todoistsync.Config) (todoistsync.Project, error) {
	return todoistsync.Project{ID: c.ProjectID, Name: "Example tasks"}, ctx.Err()
}
func (f *runtimeTodoistFetcher) Tasks(ctx context.Context, c todoistsync.Config, _, _ time.Time) ([]todoistsync.Task, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []todoistsync.Task{{ID: "33333333-3333-4333-8333-333333333333", ProjectID: c.ProjectID, Content: "Scheduled task", Priority: 1, AddedAt: at, UpdatedAt: at}}, nil
}
func readRuntimeTodoistStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/todoist/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func todoistRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/todoist/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestServiceTodoistScheduledProgress(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeTodoistFetcher()
	var resolved config.TodoistSyncConfig
	s, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TodoistSync: TodoistSyncConfig{APIOrigin: "https://api.todoist.com/", TokenEnv: " EXAMPLE_TODOIST_TOKEN "}}, serviceDeps{todoistSyncFetcherFactory: func(c config.TodoistSyncConfig) todoistsync.Fetcher { resolved = c; return f }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "https://api.todoist.com", resolved.APIOrigin)

	require.Equal(t, "EXAMPLE_TODOIST_TOKEN", resolved.TokenEnv)
	p, err := s.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, todoistRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"project_id":"project123"}}`)))
	require.Equal(t, 200, w.Code, w.Body.String())
	sub := s.broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	record := &runtimeNotionRecordingStore{Storage: s.store, recorded: make(chan db.IssueSyncStatus, 1)}
	s.store = record
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimeTodoistStatus(t, s.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "tasks", status.Status.Progress.Phase)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, todoistRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
	require.Equal(t, 409, w.Code, w.Body.String())
	close(f.release)
	finished := waitRuntimeNotionRecord(t, record.recorded)
	require.NotNil(t, finished.LastSuccessAt)
	require.Equal(t, 1, finished.LastCreated)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}
func TestServiceTodoistConfigAndExplicitCredentials(t *testing.T) {
	_, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "bad.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TodoistSync: TodoistSyncConfig{APIOrigin: "http://todoist.example"}})
	require.ErrorContains(t, err, "todoist_sync.api_origin")
	t.Setenv("KATA_TODOIST_TOKEN", "unrelated-example-secret")
	t.Setenv("EXAMPLE_TODOIST_TOKEN", "")
	s, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TodoistSync: TodoistSyncConfig{TokenEnv: "EXAMPLE_TODOIST_TOKEN"}})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	p, err := s.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, todoistRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"project_id":"project123"}}`)))
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "credentials")
	require.NotContains(t, w.Body.String(), "unrelated-example-secret")
}

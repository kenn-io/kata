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
	"go.kenn.io/kata/internal/tickticksync"
)

type runtimeTickTickFetcher struct {
	started, release chan struct{}
	once             sync.Once
}

func newRuntimeTickTickFetcher() *runtimeTickTickFetcher {
	return &runtimeTickTickFetcher{started: make(chan struct{}), release: make(chan struct{})}
}
func (f *runtimeTickTickFetcher) ForRun(context.Context, tickticksync.Config) (tickticksync.Session, error) {
	return f, nil
}
func (f *runtimeTickTickFetcher) Project(ctx context.Context) (tickticksync.Project, error) {
	return tickticksync.Project{ID: "project-1", Name: "Example tasks", Kind: "TASK"}, ctx.Err()
}
func (f *runtimeTickTickFetcher) Data(ctx context.Context) (tickticksync.ProjectData, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return tickticksync.ProjectData{}, ctx.Err()
	}
	return tickticksync.ProjectData{Project: tickticksync.Project{ID: "project-1", Name: "Example tasks", Kind: "TASK"}, Tasks: []tickticksync.Task{{ID: "task-1", ProjectID: "project-1", Title: "Scheduled task", Status: new(0)}}}, nil
}
func (f *runtimeTickTickFetcher) Task(ctx context.Context, id string) (tickticksync.Task, error) {
	return tickticksync.Task{ID: id, ProjectID: "project-1", Status: new(0)}, ctx.Err()
}
func readRuntimeTickTickStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/ticktick/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func ticktickRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/ticktick/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestServiceTickTickScheduledProgress(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeTickTickFetcher()
	var resolved config.TickTickSyncConfig
	s, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TickTickSync: TickTickSyncConfig{TokenEnv: " EXAMPLE_TICKTICK_TOKEN "}}, serviceDeps{tickTickSyncFetcherFactory: func(c config.TickTickSyncConfig) tickticksync.Fetcher { resolved = c; return f }}) //nolint:gosec // G101: TokenEnv selects an environment-variable name.
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "EXAMPLE_TICKTICK_TOKEN", resolved.TokenEnv)
	p, err := s.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, ticktickRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"project_id":"project-1"}}`)))
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
	status := readRuntimeTickTickStatus(t, s.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "tasks", status.Status.Progress.Phase)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, ticktickRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
func TestServiceTickTickConfigAndExplicitCredentials(t *testing.T) {
	_, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "bad.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TickTickSync: TickTickSyncConfig{TokenEnv: "bad-name"}})
	require.ErrorContains(t, err, "ticktick_sync.token_env")
	t.Setenv("KATA_TICKTICK_TOKEN", "unrelated-example-secret")
	t.Setenv("EXAMPLE_TICKTICK_TOKEN", "")
	s, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, TickTickSync: TickTickSyncConfig{TokenEnv: "EXAMPLE_TICKTICK_TOKEN"}})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	p, err := s.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, ticktickRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"project_id":"project-1"}}`)))
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "credentials")
	require.NotContains(t, w.Body.String(), "unrelated-example-secret")
}

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
	"go.kenn.io/kata/internal/linearsync"
)

const runtimeLinearStateID = "22222222-2222-4222-8222-222222222222"

type runtimeLinearFetcher struct {
	started, release chan struct{}
	once             sync.Once
}

func newRuntimeLinearFetcher() *runtimeLinearFetcher {
	return &runtimeLinearFetcher{started: make(chan struct{}), release: make(chan struct{})}
}
func (f *runtimeLinearFetcher) ForRun(context.Context, linearsync.Config) (linearsync.Session, error) {
	return f, nil
}
func (f *runtimeLinearFetcher) Scope(ctx context.Context, c linearsync.Config) (linearsync.Scope, error) {
	return linearsync.Scope{WorkspaceID: c.WorkspaceID, TeamID: c.TeamID, ProjectID: c.ProjectID, Name: "Example tasks"}, ctx.Err()
}
func (f *runtimeLinearFetcher) States(ctx context.Context, _ linearsync.Config) ([]linearsync.State, error) {
	return []linearsync.State{{ID: runtimeLinearStateID, Type: "started"}}, ctx.Err()
}
func (f *runtimeLinearFetcher) Issues(ctx context.Context, c linearsync.Config) ([]linearsync.Issue, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []linearsync.Issue{{ID: "33333333-3333-4333-8333-333333333333", TeamID: c.TeamID, ProjectID: c.ProjectID, StateID: runtimeLinearStateID, Identifier: "EX-1", Title: "Scheduled task", URL: "https://linear.app/example-workspace/issue/EX-1/scheduled-task", CreatedAt: at, UpdatedAt: at}}, nil
}
func readRuntimeLinearStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/linear/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func linearRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/linear/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestServiceLinearScheduledProgress(t *testing.T) {
	ctx := context.Background()
	f := newRuntimeLinearFetcher()
	var resolved config.LinearSyncConfig
	s, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, LinearSync: LinearSyncConfig{AuthType: "oauth", TokenEnv: " EXAMPLE_LINEAR_TOKEN "}}, serviceDeps{linearSyncFetcherFactory: func(c config.LinearSyncConfig) linearsync.Fetcher { resolved = c; return f }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "oauth", resolved.AuthType)
	require.Equal(t, "EXAMPLE_LINEAR_TOKEN", resolved.TokenEnv)
	p, err := s.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, linearRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"workspace_id":"44444444-4444-4444-8444-444444444444","team_id":"11111111-1111-4111-8111-111111111111"}}`)))
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
	status := readRuntimeLinearStatus(t, s.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "issues", status.Status.Progress.Phase)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, linearRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
func TestServiceLinearConfigAndExplicitCredentials(t *testing.T) {
	_, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "bad.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, LinearSync: LinearSyncConfig{AuthType: "unsupported"}})
	require.ErrorContains(t, err, "linear_sync.auth_type")
	t.Setenv("KATA_LINEAR_TOKEN", "unrelated-example-secret")
	t.Setenv("EXAMPLE_LINEAR_TOKEN", "")
	s, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, LinearSync: LinearSyncConfig{TokenEnv: "EXAMPLE_LINEAR_TOKEN"}})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	p, err := s.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, linearRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"workspace_id":"44444444-4444-4444-8444-444444444444","team_id":"11111111-1111-4111-8111-111111111111"}}`)))
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "credentials")
	require.NotContains(t, w.Body.String(), "unrelated-example-secret")
}

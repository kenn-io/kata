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
	"go.kenn.io/kata/internal/planesync"
)

const runtimePlaneStateID = "22222222-2222-4222-8222-222222222222"

type runtimePlaneFetcher struct {
	started, release chan struct{}
	once             sync.Once
}

func newRuntimePlaneFetcher() *runtimePlaneFetcher {
	return &runtimePlaneFetcher{started: make(chan struct{}), release: make(chan struct{})}
}
func (f *runtimePlaneFetcher) ForRun(context.Context, planesync.Config) (planesync.Session, error) {
	return f, nil
}
func (f *runtimePlaneFetcher) Project(ctx context.Context, c planesync.Config) (planesync.Project, error) {
	return planesync.Project{ID: c.ProjectID, Name: "Example tasks", Identifier: "EX"}, ctx.Err()
}
func (f *runtimePlaneFetcher) States(ctx context.Context, _ planesync.Config) ([]planesync.State, error) {
	return []planesync.State{{ID: runtimePlaneStateID, Group: "started"}}, ctx.Err()
}
func (f *runtimePlaneFetcher) WorkItems(ctx context.Context, c planesync.Config) ([]planesync.WorkItem, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []planesync.WorkItem{{ID: "33333333-3333-4333-8333-333333333333", ProjectID: c.ProjectID, StateID: runtimePlaneStateID, SequenceID: 1, Name: "Scheduled task", CreatedAt: at, UpdatedAt: at}}, nil
}
func readRuntimePlaneStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/plane/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func planeRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/plane/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestServicePlaneScheduledProgress(t *testing.T) {
	ctx := context.Background()
	f := newRuntimePlaneFetcher()
	var resolved config.PlaneSyncConfig
	s, err := newService(ctx, Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, PlaneSync: PlaneSyncConfig{APIOrigin: "https://plane.example/", TokenEnv: " EXAMPLE_PLANE_TOKEN "}}, serviceDeps{planeSyncFetcherFactory: func(c config.PlaneSyncConfig) planesync.Fetcher { resolved = c; return f }})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "https://plane.example", resolved.APIOrigin)
	require.Equal(t, "https://plane.example", resolved.WebOrigin)
	require.Equal(t, "EXAMPLE_PLANE_TOKEN", resolved.TokenEnv)
	p, err := s.store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, planeRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"workspace":"example-workspace","project_id":"11111111-1111-4111-8111-111111111111"}}`)))
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
	status := readRuntimePlaneStatus(t, s.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "work-items", status.Status.Progress.Phase)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, planeRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
func TestServicePlaneConfigAndExplicitCredentials(t *testing.T) {
	_, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "bad.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, PlaneSync: PlaneSyncConfig{APIOrigin: "http://plane.example"}})
	require.ErrorContains(t, err, "plane_sync.api_origin")
	t.Setenv("KATA_PLANE_TOKEN", "unrelated-example-secret")
	t.Setenv("EXAMPLE_PLANE_TOKEN", "")
	s, err := New(context.Background(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}, PlaneSync: PlaneSyncConfig{TokenEnv: "EXAMPLE_PLANE_TOKEN"}})
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	p, err := s.store.CreateProject(context.Background(), "example-project")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, planeRuntimePost(p.ID, "enable", strings.NewReader(`{"config":{"workspace":"example-workspace","project_id":"11111111-1111-4111-8111-111111111111"}}`)))
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "credentials")
	require.NotContains(t, w.Body.String(), "unrelated-example-secret")
}

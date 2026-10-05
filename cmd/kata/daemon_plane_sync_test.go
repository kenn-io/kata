package main

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
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/planesync"
)

const runtimePlaneProjectID = "11111111-1111-4111-8111-111111111111"
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
func runtimePlaneBinding(t *testing.T, store db.Storage, id int64) db.IssueSyncBinding {
	t.Helper()
	c := planesync.Config{APIOrigin: "https://api.plane.so", WebOrigin: "https://app.plane.so", Workspace: "example-workspace", ProjectID: runtimePlaneProjectID}
	raw, err := planesync.EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: id, Provider: "plane", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func readRuntimePlaneStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRuntimeNotionRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/plane/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func planeRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := newRuntimeNotionRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/plane/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDaemonPlaneScheduledProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRuntimeNotionRecordingStore(openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db")), make(chan db.IssueSyncStatus, 1))
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	runtimePlaneBinding(t, store, p.ID)
	f := newRuntimePlaneFetcher()
	tracker := issuesync.NewProgressTracker()
	workers := newDaemonWorkerGroup()
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	wake := startPlaneSyncRunner(ctx, workers, nil, store, f, daemon.NewEventPublisher(bcast, hooks.NewNoop()), nil, tracker)
	defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, PlaneSyncFetcher: f, PlaneSyncProgress: tracker})
	defer func() { require.NoError(t, server.Close()) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimePlaneStatus(t, server.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "work-items", status.Status.Progress.Phase)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, planeRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
	require.Equal(t, 409, w.Code, w.Body.String())
	for range 10000 {
		wake()
	}
	close(f.release)
	finished := waitRuntimeNotionRecord(t, store.recorded)
	require.NotNil(t, finished.LastSuccessAt)
	require.Equal(t, 1, finished.LastCreated)
	cancel()
	require.True(t, workers.Wait(context.Background()))
	require.Nil(t, readRuntimePlaneStatus(t, server.Handler(), p.ID).Status.Progress)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}

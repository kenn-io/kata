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
func runtimeTickTickBinding(t *testing.T, store db.Storage, id int64) db.IssueSyncBinding {
	t.Helper()
	c := tickticksync.Config{ProjectID: "project-1"}
	raw, err := tickticksync.EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: id, Provider: "ticktick", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
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

func TestDaemonTickTickScheduledProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRuntimeNotionRecordingStore(openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db")), make(chan db.IssueSyncStatus, 1))
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	runtimeTickTickBinding(t, store, p.ID)
	f := newRuntimeTickTickFetcher()
	tracker := issuesync.NewProgressTracker()
	workers := newDaemonWorkerGroup()
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	wake := startTickTickSyncRunner(ctx, workers, nil, store, f, daemon.NewEventPublisher(bcast, hooks.NewNoop()), nil, tracker)
	defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, TickTickSyncFetcher: f, TickTickSyncProgress: tracker})
	defer func() { require.NoError(t, server.Close()) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimeTickTickStatus(t, server.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "tasks", status.Status.Progress.Phase)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, ticktickRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
	require.Nil(t, readRuntimeTickTickStatus(t, server.Handler(), p.ID).Status.Progress)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}

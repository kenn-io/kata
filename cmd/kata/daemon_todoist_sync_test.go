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
	"go.kenn.io/kata/internal/todoistsync"
)

const runtimeTodoistProjectID = "project123"

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
	return []todoistsync.Task{{ID: "33333333-3333-4333-8333-333333333333", ProjectID: c.ProjectID, Content: "Scheduled task", Checked: new(false), Deleted: new(false), Priority: 1, AddedAt: at, UpdatedAt: at}}, nil
}
func runtimeTodoistBinding(t *testing.T, store db.Storage, id int64) db.IssueSyncBinding {
	t.Helper()
	c := todoistsync.Config{APIOrigin: "https://api.todoist.com", AccountID: "1234567", ProjectID: runtimeTodoistProjectID, HistorySince: "2026-09-01T00:00:00Z"}
	raw, err := todoistsync.EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: id, Provider: "todoist", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
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

func TestDaemonTodoistScheduledProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRuntimeNotionRecordingStore(openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db")), make(chan db.IssueSyncStatus, 1))
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	runtimeTodoistBinding(t, store, p.ID)
	f := newRuntimeTodoistFetcher()
	tracker := issuesync.NewProgressTracker()
	workers := newDaemonWorkerGroup()
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	wake := startTodoistSyncRunner(ctx, workers, nil, store, f, daemon.NewEventPublisher(bcast, hooks.NewNoop()), nil, tracker)
	defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, TodoistSyncFetcher: f, TodoistSyncProgress: tracker})
	defer func() { require.NoError(t, server.Close()) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimeTodoistStatus(t, server.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "tasks", status.Status.Progress.Phase)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, todoistRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
	require.Nil(t, readRuntimeTodoistStatus(t, server.Handler(), p.ID).Status.Progress)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}

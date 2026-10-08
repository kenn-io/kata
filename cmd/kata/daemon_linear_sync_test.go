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
	"go.kenn.io/kata/internal/linearsync"
)

const runtimeLinearProjectID = "11111111-1111-4111-8111-111111111111"
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
func runtimeLinearBinding(t *testing.T, store db.Storage, id int64) db.IssueSyncBinding {
	t.Helper()
	c := linearsync.Config{WorkspaceID: "44444444-4444-4444-8444-444444444444", TeamID: runtimeLinearProjectID}
	raw, err := linearsync.EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: id, Provider: "linear", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
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

func TestDaemonLinearScheduledProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRuntimeNotionRecordingStore(openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db")), make(chan db.IssueSyncStatus, 1))
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	runtimeLinearBinding(t, store, p.ID)
	f := newRuntimeLinearFetcher()
	tracker := issuesync.NewProgressTracker()
	workers := newDaemonWorkerGroup()
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	wake := startLinearSyncRunner(ctx, workers, nil, store, f, daemon.NewEventPublisher(bcast, hooks.NewNoop()), nil, tracker)
	defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, LinearSyncFetcher: f, LinearSyncProgress: tracker})
	defer func() { require.NoError(t, server.Close()) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimeLinearStatus(t, server.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "issues", status.Status.Progress.Phase)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, linearRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
	require.Nil(t, readRuntimeLinearStatus(t, server.Handler(), p.ID).Status.Progress)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}

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
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/twentysync"
)

const runtimeTwentyProjectID = "11111111-1111-4111-8111-111111111111"

type runtimeTwentyFetcher struct {
	started, release chan struct{}
	once             sync.Once
}

func newRuntimeTwentyFetcher() *runtimeTwentyFetcher {
	return &runtimeTwentyFetcher{started: make(chan struct{}), release: make(chan struct{})}
}
func (f *runtimeTwentyFetcher) ForRun(context.Context, twentysync.Config) (twentysync.Session, error) {
	return f, nil
}
func (f *runtimeTwentyFetcher) Workspace(ctx context.Context, _ twentysync.Config) (twentysync.Workspace, error) {
	return twentysync.Workspace{ID: "11111111-1111-4111-8111-111111111111", DisplayName: "Example tasks"}, ctx.Err()
}
func (f *runtimeTwentyFetcher) Schema(ctx context.Context, _ twentysync.Config) (twentysync.Schema, error) {
	return twentysync.Schema{StatusOptions: []string{"TODO", "IN_PROGRESS", "DONE"}}, ctx.Err()
}
func (f *runtimeTwentyFetcher) Tasks(ctx context.Context, _ twentysync.Config) ([]twentysync.Task, error) {
	f.once.Do(func() { close(f.started) })
	select {
	case <-f.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []twentysync.Task{{ID: "33333333-3333-4333-8333-333333333333", Title: "Scheduled task", CreatedAt: at, UpdatedAt: at}}, nil
}
func runtimeTwentyBinding(t *testing.T, store db.Storage, id int64) db.IssueSyncBinding {
	t.Helper()
	c := twentysync.Config{APIOrigin: "https://api.twenty.so", WebOrigin: "https://app.twenty.so", WorkspaceID: runtimeTwentyProjectID}
	raw, err := twentysync.EncodeConfig(c)
	require.NoError(t, err)
	b, err := store.UpsertIssueSyncBinding(context.Background(), db.UpsertIssueSyncBindingParams{ProjectID: id, Provider: "twenty", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), DisplayName: "Example tasks", Config: raw, IntervalSeconds: 300})
	require.NoError(t, err)
	return b
}
func readRuntimeTwentyStatus(t *testing.T, h http.Handler, id int64) runtimeNotionStatus {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRuntimeNotionRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issue-sync/twenty/status", id), nil))
	require.Equal(t, 200, w.Code, w.Body.String())
	var s runtimeNotionStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &s))
	return s
}
func twentyRuntimePost(id int64, action string, body io.Reader) *http.Request {
	r := newRuntimeNotionRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issue-sync/twenty/%s", id, action), body)
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDaemonTwentyScheduledProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newRuntimeNotionRecordingStore(openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db")), make(chan db.IssueSyncStatus, 1))
	defer func() { require.NoError(t, store.Close()) }()
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	runtimeTwentyBinding(t, store, p.ID)
	f := newRuntimeTwentyFetcher()
	tracker := issuesync.NewProgressTracker()
	workers := newDaemonWorkerGroup()
	bcast := daemon.NewEventBroadcaster()
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	wake := startTwentySyncRunner(ctx, workers, nil, store, f, daemon.NewEventPublisher(bcast, hooks.NewNoop()), nil, tracker)
	defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, TwentySyncFetcher: f, TwentySyncProgress: tracker})
	defer func() { require.NoError(t, server.Close()) }()
	waitRuntimeNotion(t, f.started)
	status := readRuntimeTwentyStatus(t, server.Handler(), p.ID)
	require.NotNil(t, status.Status.Progress)
	require.Equal(t, "tasks", status.Status.Progress.Phase)
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, twentyRuntimePost(p.ID, "once", strings.NewReader(`{}`)))
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
	require.Nil(t, readRuntimeTwentyStatus(t, server.Handler(), p.ID).Status.Progress)
	select {
	case msg := <-sub.Ch:
		require.Equal(t, "issue.created", msg.Event.Type)
	case <-time.After(5 * time.Second):
		t.Fatal("missing event")
	}
}

func TestTwentyWorkerDrainAndShutdown(t *testing.T) {
	t.Run("admitted events fork during drain", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := openKataTestDB(t, filepath.Join(t.TempDir(), "runtime.db"))
			defer func() { require.NoError(t, store.Close()) }()
			ctx, cancel := context.WithCancel(t.Context())
			workers := newDaemonWorkerGroup()
			defer func() { cancel(); require.True(t, workers.Wait(context.Background())) }()
			project, err := store.CreateProject(ctx, "example-project")
			require.NoError(t, err)
			runtimeTwentyBinding(t, store, project.ID)
			fetcher := newRuntimeTwentyFetcher()
			idle := make(chan struct{})
			controller := daemon.NewIdleController(time.Second, func() { close(idle) })
			controller.Start()
			sink := &runtimeNotionForkSink{children: make(chan *activity.Lease, 1)}
			wake := startTwentySyncRunner(ctx, workers, controller.WaitableDrainAdmission(), store, fetcher, daemon.NewEventPublisher(nil, sink), nil, issuesync.NewProgressTracker())
			synctest.Wait()
			select {
			case <-fetcher.started:
			default:
				require.FailNow(t, "content did not start")
			}
			time.Sleep(time.Second)
			synctest.Wait()
			require.Equal(t, daemon.IdleStateBlocked, controller.Snapshot().State)
			_, admitted, _ := controller.WaitableDrainAdmission()()
			require.False(t, admitted)
			wake()
			close(fetcher.release)
			synctest.Wait()
			status, err := store.IssueSyncStatusByProject(ctx, project.ID)
			require.NoError(t, err)
			require.NotNil(t, status.LastSuccessAt)
			var child *activity.Lease
			select {
			case child = <-sink.children:
			default:
				require.FailNow(t, "committed event did not fork hook delivery")
			}
			require.Equal(t, daemon.IdleStateBlocked, controller.Snapshot().State)
			child.Release()
			synctest.Wait()
			select {
			case <-idle:
			default:
				require.Fail(t, "idle shutdown did not resume after event delivery")
			}
		})
	})
}

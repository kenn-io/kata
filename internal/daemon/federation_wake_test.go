package daemon_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

// wakeLog records the wakes the forwarder requests, in order. A project wake
// is logged as its ID; a wake for every spoke as -1. The next project wake
// blocks until holdProject closes, and the next wake for every spoke until
// holdAll closes.
type wakeLog struct {
	mu          sync.Mutex
	wakes       []int64
	holdProject chan struct{}
	holdAll     chan struct{}
}

func (w *wakeLog) record(id int64, hold *chan struct{}) {
	w.mu.Lock()
	w.wakes = append(w.wakes, id)
	held := *hold
	*hold = nil
	w.mu.Unlock()
	if held != nil {
		<-held
	}
}

func (w *wakeLog) project(id int64) { w.record(id, &w.holdProject) }

func (w *wakeLog) all() { w.record(-1, &w.holdAll) }

func (w *wakeLog) taken() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.wakes
	w.wakes = nil
	return out
}

func openWakeStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func localEvent(store db.Storage, projectID int64) daemon.StreamMsg {
	return daemon.NewEventMsg(projectID, db.Event{ProjectID: projectID, Type: "issue.updated", OriginInstanceUID: store.InstanceUID()})
}

func TestFederationWakesNameTheProjectOfEachLocalEvent(t *testing.T) {
	store := openWakeStore(t)
	source, err := store.CreateProject(t.Context(), "source-project")
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		broadcaster := daemon.NewEventBroadcaster()
		log := &wakeLog{}
		ctx, cancel := context.WithCancel(t.Context())
		forward := daemon.SubscribeFederationWakes(broadcaster, store, log.project, log.all)
		done := make(chan struct{})
		go func() {
			defer close(done)
			forward(ctx)
		}()

		broadcaster.Broadcast(daemon.NewEventMsg(7, db.Event{ProjectID: 7, Type: "issue.created", OriginInstanceUID: store.InstanceUID()}))
		broadcaster.Broadcast(daemon.NewEventMsg(8, db.Event{ProjectID: 8, Type: "issue.created", OriginInstanceUID: "hub-instance"}))
		broadcaster.Broadcast(localEvent(store, 9))
		broadcaster.Broadcast(daemon.StreamMsg{Kind: daemon.StreamKindReset, ResetID: 5, ProjectID: 9})
		synctest.Wait()
		assert.Equal(t, []int64{7, 9}, log.taken(), "pulled events and resets wake nothing")

		broadcaster.Broadcast(daemon.NewEventMsg(0, db.Event{Type: "project.created", OriginInstanceUID: store.InstanceUID()}))
		synctest.Wait()
		assert.Equal(t, []int64{-1}, log.taken(), "an event without a project wakes every spoke")

		broadcaster.Broadcast(daemon.NewEventMsg(12, db.Event{
			ProjectID: 12, Type: "issue.moved", OriginInstanceUID: store.InstanceUID(),
			Payload: `{"from_project_uid":"` + source.UID + `"}`,
		}))
		synctest.Wait()
		assert.Equal(t, []int64{-1}, log.taken(), "a move wakes every spoke")

		cancel()
		<-done
	})
}

// A write that commits between subscribing and starting the forwarding loop
// still wakes its project, so the daemon can subscribe before it starts the
// runner.
func TestFederationWakesSeeWritesBeforeTheLoopStarts(t *testing.T) {
	store := openWakeStore(t)
	synctest.Test(t, func(t *testing.T) {
		broadcaster := daemon.NewEventBroadcaster()
		log := &wakeLog{}
		forward := daemon.SubscribeFederationWakes(broadcaster, store, log.project, log.all)
		broadcaster.Broadcast(localEvent(store, 4))

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			forward(ctx)
		}()
		synctest.Wait()
		assert.Equal(t, []int64{4}, log.taken())

		cancel()
		<-done
	})
}

func TestFederationWakesWakeEverySpokeAfterMissingEvents(t *testing.T) {
	store := openWakeStore(t)
	synctest.Test(t, func(t *testing.T) {
		broadcaster := daemon.NewEventBroadcaster()
		firstWake, catchUp := make(chan struct{}), make(chan struct{})
		log := &wakeLog{holdProject: firstWake, holdAll: catchUp}
		ctx, cancel := context.WithCancel(t.Context())
		forward := daemon.SubscribeFederationWakes(broadcaster, store, log.project, log.all)
		done := make(chan struct{})
		go func() {
			defer close(done)
			forward(ctx)
		}()

		broadcaster.Broadcast(localEvent(store, 1))
		synctest.Wait()
		// The forwarder is busy with the first wake, so these overflow its
		// subscription and the broadcaster drops it.
		for range 300 {
			broadcaster.Broadcast(localEvent(store, 2))
		}
		close(firstWake)
		synctest.Wait()
		wakes := log.taken()
		require.NotEmpty(t, wakes)
		assert.Equal(t, int64(-1), wakes[len(wakes)-1], "events lost to the overflow wake every spoke")

		// The forwarder subscribed again before that wake, so a write during
		// it still wakes its project.
		broadcaster.Broadcast(localEvent(store, 3))
		close(catchUp)
		synctest.Wait()
		assert.Equal(t, []int64{3}, log.taken())

		cancel()
		<-done
	})
}

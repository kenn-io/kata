package daemon_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// wakeLog records the wakes ForwardFederationWakes requests, in order. A
// project wake is logged as its ID; a wake for every spoke as -1.
type wakeLog struct {
	mu    sync.Mutex
	wakes []int64
	block chan struct{}
}

func (w *wakeLog) project(id int64) {
	w.mu.Lock()
	w.wakes = append(w.wakes, id)
	block := w.block
	w.block = nil
	w.mu.Unlock()
	if block != nil {
		<-block
	}
}

func (w *wakeLog) all() { w.project(-1) }

func (w *wakeLog) taken() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.wakes
	w.wakes = nil
	return out
}

func TestForwardFederationWakesNamesTheProjectOfEachLocalEvent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broadcaster := daemon.NewEventBroadcaster()
		log := &wakeLog{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			daemon.ForwardFederationWakes(ctx, broadcaster, "local-instance", log.project, log.all)
		}()
		synctest.Wait()

		broadcaster.Broadcast(daemon.NewEventMsg(7, db.Event{ProjectID: 7, Type: "issue.created", OriginInstanceUID: "local-instance"}))
		broadcaster.Broadcast(daemon.NewEventMsg(8, db.Event{ProjectID: 8, Type: "issue.created", OriginInstanceUID: "hub-instance"}))
		broadcaster.Broadcast(daemon.NewEventMsg(9, db.Event{ProjectID: 9, Type: "issue.updated", OriginInstanceUID: "local-instance"}))
		broadcaster.Broadcast(daemon.StreamMsg{Kind: daemon.StreamKindReset, ResetID: 5, ProjectID: 9})
		synctest.Wait()
		assert.Equal(t, []int64{7, 9}, log.taken(), "pulled events and resets wake nothing")

		broadcaster.Broadcast(daemon.NewEventMsg(0, db.Event{Type: "project.created", OriginInstanceUID: "local-instance"}))
		synctest.Wait()
		assert.Equal(t, []int64{-1}, log.taken(), "an event without a project wakes every spoke")

		cancel()
		<-done
	})
}

func TestForwardFederationWakesWakesEverySpokeAfterMissingEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broadcaster := daemon.NewEventBroadcaster()
		release := make(chan struct{})
		log := &wakeLog{block: release}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			daemon.ForwardFederationWakes(ctx, broadcaster, "local-instance", log.project, log.all)
		}()
		synctest.Wait()

		local := func(projectID int64) daemon.StreamMsg {
			return daemon.NewEventMsg(projectID, db.Event{ProjectID: projectID, Type: "issue.updated", OriginInstanceUID: "local-instance"})
		}
		broadcaster.Broadcast(local(1))
		synctest.Wait()
		// The forwarder is busy with the first wake, so these overflow its
		// subscription and the broadcaster drops it.
		for range 300 {
			broadcaster.Broadcast(local(2))
		}
		close(release)
		synctest.Wait()
		wakes := log.taken()
		require.NotEmpty(t, wakes)
		assert.Equal(t, int64(-1), wakes[len(wakes)-1], "events lost to the overflow wake every spoke")

		broadcaster.Broadcast(local(3))
		synctest.Wait()
		assert.Equal(t, []int64{3}, log.taken(), "the forwarder keeps forwarding after an overflow")

		cancel()
		<-done
	})
}

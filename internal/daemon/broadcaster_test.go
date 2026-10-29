package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/stacktest"
)

func TestBroadcaster_SubscribeAndUnsubLifecycle(t *testing.T) {
	b := daemon.NewEventBroadcaster()
	sub := b.Subscribe(daemon.SubFilter{})
	sub.Unsub()
	// Calling Unsub twice must be safe — closes only once.
	sub.Unsub()
	assertChannelClosed(t, sub.Ch, time.Second, "sub.Ch after Unsub")
}

func TestBroadcaster_BroadcastFansToMatchingFiltersOnly(t *testing.T) {
	b := daemon.NewEventBroadcaster()
	all := b.Subscribe(daemon.SubFilter{})
	a := b.Subscribe(daemon.SubFilter{ProjectID: 1})
	other := b.Subscribe(daemon.SubFilter{ProjectID: 2})
	defer all.Unsub()
	defer a.Unsub()
	defer other.Unsub()

	broadcastEvent(b, 1, 100)

	gotAll := receiveMsg(t, all.Ch, time.Second, "cross-project subscriber")
	assert.Equal(t, daemon.StreamKindEvent, gotAll.Kind)
	assert.Equal(t, int64(100), gotAll.Event.ID)

	gotA := receiveMsg(t, a.Ch, time.Second, "project-1 subscriber")
	assert.Equal(t, int64(100), gotA.Event.ID)

	assertNoReceive(t, other.Ch, 50*time.Millisecond, "project-2 subscriber must not receive a project-1 event")
}

func TestBroadcaster_ResetFansToAllMatchingFilters(t *testing.T) {
	b := daemon.NewEventBroadcaster()
	all := b.Subscribe(daemon.SubFilter{})
	a := b.Subscribe(daemon.SubFilter{ProjectID: 1})
	defer all.Unsub()
	defer a.Unsub()

	b.Broadcast(daemon.NewResetMsg(1, 999))

	for i, ch := range []<-chan daemon.StreamMsg{all.Ch, a.Ch} {
		got := receiveMsg(t, ch, time.Second, "reset subscriber")
		assert.Equalf(t, daemon.StreamKindReset, got.Kind, "subscriber %d", i)
		assert.Equalf(t, int64(999), got.ResetID, "subscriber %d", i)
	}
}

func TestBroadcaster_OverflowDisconnectsSlowSubscriberOnly(t *testing.T) {
	b := daemon.NewEventBroadcaster()
	slow := b.Subscribe(daemon.SubFilter{})
	fast := b.Subscribe(daemon.SubFilter{})
	defer fast.Unsub()
	// Don't Unsub slow — broadcast saturates its buffer (256) and we expect
	// the broadcaster to close it.

	for i := range int64(300) {
		broadcastEvent(b, 1, i+1)
	}

	assertChannelClosed(t, slow.Ch, time.Second, "slow subscriber's channel must close on overflow")

	// fast must still be live: drain it and assert at least one delivery.
	got := 0
loop:
	for {
		select {
		case _, ok := <-fast.Ch:
			if !ok {
				break loop
			}
			got++
		default:
			break loop
		}
	}
	assert.Greater(t, got, 0, "fast subscriber should still be receiving")
}

func TestBroadcaster_RaceFuzz(t *testing.T) {
	// -race coverage for concurrent Subscribe/Broadcast/Unsub.
	// The test asserts no goroutine leaks (every Unsub completes) and no
	// data races (caught by -race). Without -race, the test still verifies
	// the wg.Wait() returns within the deadline (no deadlock).
	b := daemon.NewEventBroadcaster()
	var wg sync.WaitGroup
	const N = 200
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := b.Subscribe(daemon.SubFilter{ProjectID: int64(i % 5)})
			drain := make(chan struct{})
			go func() {
				for range sub.Ch { //nolint:revive // empty body: drain only, values discarded
				}
				close(drain)
			}()
			time.Sleep(time.Microsecond)
			sub.Unsub()
			<-drain
		}(i)
	}
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			evt := &db.Event{ID: int64(i + 1), ProjectID: int64(i % 5), Type: "issue.created"}
			b.Broadcast(daemon.NewEventMsg(evt.ProjectID, *evt))
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// expected: all goroutines completed cleanly
	case <-time.After(10 * time.Second):
		stacktest.Dump(t)
		t.Fatal("race fuzz did not complete within 10s — possible deadlock")
	}
}

func TestBroadcaster_MoveWakesSourceWithoutUnrelatedOverflow(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	source := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 1, ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAVA"})
	destination := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 2, ProjectUID: "01ARZ3NDEKTSV4RRFFQ69G5FAVB"})
	defer source.Unsub()
	defer destination.Unsub()
	for i := range 300 {
		broadcaster.Broadcast(daemon.NewEventMsg(3, db.Event{ID: int64(i + 1), ProjectID: 3, Type: "issue.updated", Payload: `{"from_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVA"}`}))
	}
	broadcaster.Broadcast(daemon.NewResetMsg(3, 300))
	broadcaster.Broadcast(daemon.NewEventMsg(1, db.Event{ID: 301, ProjectID: 1, Type: "issue.updated"}))
	broadcaster.Broadcast(daemon.NewEventMsg(2, db.Event{ID: 302, ProjectID: 2, Type: "issue.moved", Payload: `{"from_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVA","to_project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAVB"}`}))
	own := receiveMsg(t, source.Ch, time.Second, "source issue event after unrelated burst")
	assert.Equal(t, int64(301), own.Event.ID)
	moved := receiveMsg(t, source.Ch, time.Second, "source move departure")
	assert.Equal(t, int64(302), moved.Event.ID)
	arrived := receiveMsg(t, destination.Ch, time.Second, "destination move arrival")
	assert.Equal(t, int64(302), arrived.Event.ID)
	select {
	case extra := <-source.Ch:
		t.Fatalf("unexpected source delivery: %+v", extra)
	default:
	}
	select {
	case extra := <-destination.Ch:
		t.Fatalf("unexpected destination delivery: %+v", extra)
	default:
	}
}

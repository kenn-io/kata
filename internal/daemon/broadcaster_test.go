package daemon_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
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
	assert.Equal(t, "event", gotAll.Kind)
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

	b.Broadcast(daemon.StreamMsg{Kind: "reset", ResetID: 999, ProjectID: 1})

	for i, ch := range []<-chan daemon.StreamMsg{all.Ch, a.Ch} {
		got := receiveMsg(t, ch, time.Second, "reset subscriber")
		assert.Equalf(t, "reset", got.Kind, "subscriber %d", i)
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
		case <-time.After(20 * time.Millisecond):
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
			b.Broadcast(daemon.StreamMsg{Kind: "event", Event: evt, ProjectID: evt.ProjectID})
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// expected: all goroutines completed cleanly
	case <-time.After(10 * time.Second):
		t.Fatal("race fuzz did not complete within 10s — possible deadlock")
	}
}

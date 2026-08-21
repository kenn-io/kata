package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

type parentForkSink struct {
	admitted bool
}

func (*parentForkSink) Enqueue(db.Event) {}

func (s *parentForkSink) EnqueueFrom(_ db.Event, acquire hooks.AcquireActivity) {
	lease, admitted := acquire()
	s.admitted = admitted
	if lease != nil {
		lease.Release()
	}
}

func TestBlockedIdleDrainCanHandProtectionToCausedHook(t *testing.T) {
	clock := newIdleTestClock(time.Date(2026, time.August, 17, 12, 0, 0, 0, time.UTC))
	idle := make(chan struct{}, 1)
	controller := newIdleControllerWithClock(time.Minute, func() { idle <- struct{}{} }, clock)
	controller.Start()
	parent, admitted := controller.TryDrain()
	require.True(t, admitted)
	clock.Advance(time.Minute)
	require.Equal(t, IdleStateBlocked, controller.Snapshot().State)

	sink := &parentForkSink{}
	enqueueHookWithDrain(sink, db.Event{ID: 1, Type: "issue.created"}, activityLeaseFromIdle(parent))
	require.True(t, sink.admitted)
	requireNoIdleCallback(t, idle)
	parent.Release()
	select {
	case <-idle:
	default:
		t.Fatal("idle callback did not run after parent and caused hook released")
	}
}

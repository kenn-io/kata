package daemon_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestMCPEventsSubscriberWakesOnBroadcastsAndClosesOnCancel(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	wake, cancel := daemon.NewMCPEventsSubscriber(broadcaster).Subscribe()

	// Any broadcast wakes, and a burst coalesces into at least one wake.
	for range 5 {
		broadcaster.Broadcast(daemon.NewEventMsg(1, db.Event{ID: 1}))
	}
	broadcaster.Broadcast(daemon.NewResetMsg(0, 10))
	select {
	case _, ok := <-wake:
		require.True(t, ok, "a broadcast delivers a value, not a close")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "no wake after a broadcast")
	}

	cancel()
	cancel() // idempotent
	//nolint:kennlint // Native broadcaster cancellation runs outside a synctest bubble; bounded polling observes channel closure.
	require.Eventually(t, func() bool {
		for {
			select {
			case _, ok := <-wake:
				if !ok {
					return true
				}
			default:
				return false
			}
		}
	}, 2*time.Second, 5*time.Millisecond, "cancel closes the wake channel")

	// A cancelled subscription is gone: later broadcasts neither block nor panic.
	assert.NotPanics(t, func() { broadcaster.Broadcast(daemon.NewEventMsg(1, db.Event{ID: 2})) })
}

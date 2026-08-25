package daemon_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// TestSSE_OutOfOrderBroadcastsEmitInIDOrder pins the wakeup-and-requery
// guarantee: even if Broadcast(102) fires before Broadcast(101), the SSE
// consumer sees frame id=101 first, then id=102. The DB is the ordering
// authority; the broadcaster only signals "something changed at or below N".
func TestSSE_OutOfOrderBroadcastsEmitInIDOrder(t *testing.T) {
	env := testenv.New(t)
	pid, _, framer := setupLiveSSE(t, env)

	// Now in live phase. Insert two events directly via DB so the handler-side
	// broadcast does NOT fire; broadcast manually in inverted order to pin the
	// wakeup-and-requery ordering claim.
	_, evt1 := mkIssueWithEvent(t, env, pid, "first")
	_, evt2 := mkIssueWithEvent(t, env, pid, "second")

	// Inverted: evt2 first, then evt1.
	env.Broadcaster.Broadcast(daemon.NewEventMsg(pid, evt2))
	env.Broadcaster.Broadcast(daemon.NewEventMsg(pid, evt1))

	first, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok)
	second, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok)
	assert.Equal(t, strconv.FormatInt(evt1.ID, 10), first.id,
		"first frame must be the lower id, regardless of broadcast order")
	assert.Equal(t, strconv.FormatInt(evt2.ID, 10), second.id)
}

// TestSSE_LivePhaseChecksPurgeResetBeforeReplay pins the cross-cutting fix
// that surfaces sync.reset_required even when the corresponding "reset"
// broadcast lost the race to a post-purge "event" broadcast. The handler
// re-checks PurgeResetCheck on every event wakeup so the client cannot
// receive a post-purge frame and silently advance past the reset cursor.
func TestSSE_LivePhaseChecksPurgeResetBeforeReplay(t *testing.T) {
	env := testenv.New(t)
	pid, sentinelIssue, framer := setupLiveSSE(t, env)

	// Now create a second issue and purge the sentinel directly via DB so
	// the handler-side broadcasts do NOT fire. Then broadcast ONLY an
	// "event" message (simulating the broadcaster reordering: reset lost
	// the race). The handler must still emit a reset because
	// PurgeResetCheck sees the purge committed.
	_, evt2 := mkIssueWithEvent(t, env, pid, "post-purge")
	_, err := env.DB.PurgeIssue(context.Background(), sentinelIssue.ID, "tester", nil)
	require.NoError(t, err)

	env.Broadcaster.Broadcast(daemon.NewEventMsg(pid, evt2))

	frame, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok)
	assert.Equal(t, "sync.reset_required", frame.event,
		"live phase must surface the reset before replaying post-purge events")
}

// TestBroadcaster_ConcurrentSubscribeBroadcastUnsub is a -race fuzz of
// concurrent Subscribe/Broadcast/Unsub. It asserts the broadcaster doesn't
// deadlock, panic, or leak goroutines.
func TestBroadcaster_ConcurrentSubscribeBroadcastUnsub(_ *testing.T) {
	b := daemon.NewEventBroadcaster()
	var wg sync.WaitGroup
	const N = 100
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := b.Subscribe(daemon.SubFilter{ProjectID: int64(i % 3)})
			drain := make(chan struct{})
			go func() {
				for range sub.Ch { //nolint:revive // empty body: drain only, values discarded
				}
				close(drain)
			}()
			time.Sleep(time.Microsecond * time.Duration(i%5))
			sub.Unsub()
			<-drain
		}(i)
	}
	for i := range N {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			evt := db.Event{ID: int64(i + 1), ProjectID: int64(i % 3), Type: "issue.created"}
			b.Broadcast(daemon.NewEventMsg(evt.ProjectID, evt))
		}(i)
	}
	wg.Wait()
}

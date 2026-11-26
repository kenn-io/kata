package federation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// BenchmarkLocalWriteWakeToPass measures one local write with 50 bound
// projects, from the write to the end of the sync pass it wakes, and reports
// the HTTP requests the hub receives for it.
func BenchmarkLocalWriteWakeToPass(b *testing.B) {
	const boundProjects = 50
	hubStore, hubURL := newFederationNode(b)
	spoke, _ := newFederationNode(b)
	b.Setenv("KATA_HOME", b.TempDir())
	hub := newCountingHub(b, hubURL)
	written := bindSpokeProjects(b, hubStore, spoke, hub.URL, boundProjects)[0].spoke

	var results atomic.Int64
	firstPass := make(chan struct{})
	// writtenTurns receives a value when the written project's turn ends. It
	// holds at most one.
	writtenTurns := make(chan struct{}, 1)
	wake := NewWakeQueue()
	runner := &Runner{
		DB:       spoke,
		Interval: time.Hour,
		Wake:     wake,
		OnSpokeResult: func(projectID int64, _ error) {
			if results.Add(1) == boundProjects {
				close(firstPass)
			}
			if projectID == written.ID {
				select {
				case writtenTurns <- struct{}{}:
				default:
				}
			}
		},
		OnError: func(err error) { b.Errorf("federation runner: %v", err) },
	}
	debounces := make(chan heldTimer, 1)
	runner.SetDebounceTimerForTest(func(time.Duration) Timer {
		timer := heldTimer{fire: make(chan time.Time, 1)}
		debounces <- timer
		return timer
	})
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	// Connect the runner to local events the way the daemon does.
	events := daemon.NewEventBroadcaster()
	go daemon.SubscribeFederationWakes(events, spoke, wake.WakeProject, wake.WakeAll)(ctx)
	go func() { _ = runner.Run(ctx) }()
	select {
	case <-firstPass:
	case <-time.After(time.Minute):
		b.Fatal("the first pass did not finish")
	}
	hub.reset()

	b.ResetTimer()
	for range b.N {
		// Record a local issue and broadcast its event, as the daemon's
		// publisher does after a local write commits.
		_, event, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: written.ID, Title: "benchmark write", Author: "tester"})
		require.NoError(b, err)
		events.Broadcast(daemon.NewEventMsg(written.ID, event))
		// The runner starts this debounce only after the previous pass ended,
		// so no push from that pass can still arrive.
		timer := <-debounces
		select {
		case <-writtenTurns:
		default:
		}
		timer.fire <- time.Now()
		// The written project's turn pushed this write and ended, so the next
		// write wakes a new pass instead of joining this turn's push.
		<-writtenTurns
	}
	// The runner debounces this wake only after the last measured pass ended.
	// The debounce never fires, so the wake starts no pass.
	wake.WakeAll()
	<-debounces
	b.StopTimer()
	total, _ := hub.counts()
	b.ReportMetric(float64(total)/float64(b.N), "hub-requests/write")
}

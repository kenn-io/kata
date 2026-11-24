package federation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

const benchBoundProjects = 50

type wakeBench struct {
	hub       *countingHub
	spoke     *sqlitestore.Store
	bound     []boundSpokeProject
	events    *daemon.EventBroadcaster
	runner    *Runner
	results   atomic.Int64
	firstPass chan struct{}
	// writtenTurns receives a value when the written project's turn ends. It
	// holds at most one.
	writtenTurns chan struct{}
}

func newWakeBench(b *testing.B) *wakeBench {
	b.Helper()
	hub, hubURL := newFederationNode(b)
	spoke, _ := newFederationNode(b)
	b.Setenv("KATA_HOME", b.TempDir())
	bench := &wakeBench{hub: newCountingHub(b, hubURL), spoke: spoke, events: daemon.NewEventBroadcaster(), firstPass: make(chan struct{}), writtenTurns: make(chan struct{}, 1)}
	bench.bound = bindSpokeProjects(b, hub, spoke, bench.hub.URL, benchBoundProjects)
	bench.runner = &Runner{
		DB: spoke,
		OnSpokeResult: func(projectID int64, _ error) {
			if bench.results.Add(1) == benchBoundProjects {
				close(bench.firstPass)
			}
			if projectID == bench.bound[0].spoke.ID {
				select {
				case bench.writtenTurns <- struct{}{}:
				default:
				}
			}
		},
		OnError: func(err error) { b.Errorf("federation runner: %v", err) },
	}
	return bench
}

// write records one local issue and broadcasts its event, as the daemon's
// publisher does after a local write commits.
func (w *wakeBench) write(b *testing.B, title string) {
	b.Helper()
	project := w.bound[0].spoke
	_, event, err := w.spoke.CreateIssue(b.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: title, Author: "tester"})
	require.NoError(b, err)
	w.events.Broadcast(daemon.NewEventMsg(project.ID, event))
}

func (w *wakeBench) awaitInitialPass(b *testing.B) {
	b.Helper()
	select {
	case <-w.firstPass:
	case <-time.After(time.Minute):
		b.Fatal("the first pass did not finish")
	}
}

// BenchmarkLocalWriteWakeToPass measures one local write with 50 bound
// projects, from the write to the end of the sync pass it wakes, and reports
// the HTTP requests the hub receives for it.
func BenchmarkLocalWriteWakeToPass(b *testing.B) {
	bench := newWakeBench(b)
	bench.runner.Interval = time.Hour
	debounces := make(chan heldTimer, 1)
	bench.runner.SetDebounceTimerForTest(func(time.Duration) Timer {
		timer := heldTimer{fire: make(chan time.Time, 1)}
		debounces <- timer
		return timer
	})
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	wakeWithoutWrite := attachBenchWakes(ctx, bench.runner, bench.events, bench.spoke)
	go func() { _ = bench.runner.Run(ctx) }()
	bench.awaitInitialPass(b)
	bench.hub.reset()

	b.ResetTimer()
	for range b.N {
		bench.write(b, "benchmark write")
		// The runner starts this debounce only after the previous pass ended,
		// so no push from that pass can still arrive.
		timer := <-debounces
		clearSignal(bench.writtenTurns)
		timer.fire <- time.Now()
		// The written project's turn pushed this write and ended, so the next
		// write wakes a new pass instead of joining this turn's push.
		<-bench.writtenTurns
	}
	// The runner debounces this wake only after the last measured pass ended.
	// The debounce never fires, so the wake starts no pass.
	wakeWithoutWrite()
	<-debounces
	b.StopTimer()
	total, _ := bench.hub.counts()
	b.ReportMetric(float64(total)/float64(b.N), "hub-requests/write")
}

func clearSignal(signal chan struct{}) {
	select {
	case <-signal:
	default:
	}
}

// attachBenchWakes connects runner to the local events on broadcaster the way
// the daemon does. It returns a function that wakes the runner without a
// write.
func attachBenchWakes(ctx context.Context, runner *Runner, broadcaster *daemon.EventBroadcaster, store db.Storage) func() {
	wake := NewWakeQueue()
	runner.Wake = wake
	go daemon.SubscribeFederationWakes(broadcaster, store, wake.WakeProject, wake.WakeAll)(ctx)
	return wake.WakeAll
}

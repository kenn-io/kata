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
}

func newWakeBench(b *testing.B) *wakeBench {
	b.Helper()
	hub, hubURL := newFederationNode(b)
	spoke, _ := newFederationNode(b)
	b.Setenv("KATA_HOME", b.TempDir())
	bench := &wakeBench{hub: newCountingHub(b, hubURL), spoke: spoke, events: daemon.NewEventBroadcaster(), firstPass: make(chan struct{})}
	bench.bound = bindSpokeProjects(b, hub, spoke, bench.hub.URL, benchBoundProjects)
	bench.runner = &Runner{
		DB: spoke,
		OnSpokeResult: func(int64, error) {
			if bench.results.Add(1) == benchBoundProjects {
				close(bench.firstPass)
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
	attachBenchWakes(ctx, bench.runner, bench.events, bench.spoke.InstanceUID())
	go func() { _ = bench.runner.Run(ctx) }()
	bench.awaitInitialPass(b)
	bench.hub.reset()

	b.ResetTimer()
	for range b.N {
		bench.write(b, "benchmark write")
		// The runner starts this debounce only after the previous pass ended.
		timer := <-debounces
		timer.fire <- time.Now()
	}
	// One more write proves the last measured pass ended; its own pass never
	// starts because its debounce is never fired.
	bench.write(b, "sentinel write")
	<-debounces
	b.StopTimer()
	total, _ := bench.hub.counts()
	b.ReportMetric(float64(total)/float64(b.N), "hub-requests/write")
}

// BenchmarkSpokeWritingEveryTenSeconds runs a spoke with 50 bound projects
// that writes once every 10 seconds, for one hour of scaled time per
// iteration, with the default 30 second interval and 50 ms debounce, and
// reports the HTTP requests the hub receives per hour. Run it with
// -benchtime=1x; each iteration takes 3600/scale seconds.
func BenchmarkSpokeWritingEveryTenSeconds(b *testing.B) {
	const scale = 10
	bench := newWakeBench(b)
	bench.runner.Interval = 30 * time.Second / scale
	bench.runner.Debounce = 50 * time.Millisecond / scale
	ctx, cancel := context.WithCancel(b.Context())
	defer cancel()
	attachBenchWakes(ctx, bench.runner, bench.events, bench.spoke.InstanceUID())
	go func() { _ = bench.runner.Run(ctx) }()
	bench.awaitInitialPass(b)

	b.ResetTimer()
	var total int
	for range b.N {
		bench.hub.reset()
		writes := time.NewTicker(10 * time.Second / scale)
		hour := time.NewTimer(time.Hour / scale)
	simulate:
		for {
			select {
			case <-writes.C:
				bench.write(b, "periodic write")
			case <-hour.C:
				break simulate
			}
		}
		writes.Stop()
		requests, _ := bench.hub.counts()
		total += requests
	}
	b.StopTimer()
	b.ReportMetric(float64(total)/float64(b.N), "hub-requests/hour")
}

// attachBenchWakes connects runner to the local events on broadcaster the way
// the daemon does.
func attachBenchWakes(ctx context.Context, runner *Runner, broadcaster *daemon.EventBroadcaster, localInstanceUID string) {
	wake := NewWakeQueue()
	runner.Wake = wake
	go daemon.ForwardFederationWakes(ctx, broadcaster, localInstanceUID, wake.WakeProject, wake.WakeAll)
}

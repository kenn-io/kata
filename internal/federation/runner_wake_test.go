package federation

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// spokeResults records which spokes the runner finished a turn for, in order.
type spokeResults struct {
	mu   sync.Mutex
	seen []int64
	next chan int64
}

func newSpokeResults() *spokeResults {
	return &spokeResults{next: make(chan int64, 128)}
}

func (s *spokeResults) record(projectID int64, _ error) {
	s.mu.Lock()
	s.seen = append(s.seen, projectID)
	s.mu.Unlock()
	s.next <- projectID
}

func (s *spokeResults) await(t *testing.T, want ...int64) {
	t.Helper()
	remaining := map[int64]bool{}
	for _, id := range want {
		remaining[id] = true
	}
	for len(remaining) > 0 {
		select {
		case id := <-s.next:
			delete(remaining, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("spoke turns still missing: %v", remaining)
		}
	}
}

func (s *spokeResults) since(start int) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.seen[start:]...)
}

func (s *spokeResults) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// newWakeRunner returns a runner whose wake debounce lasts until the test
// fires it. The runner starts a debounce only while it waits between passes,
// so receiving one also proves the previous pass finished.
func newWakeRunner(t *testing.T, spoke *testenv.Env, wake *WakeQueue, results *spokeResults) (*Runner, <-chan heldTimer) {
	t.Helper()
	runner := &Runner{
		DB:            spoke.DB,
		Interval:      time.Hour,
		Wake:          wake,
		OnSpokeResult: results.record,
		OnError:       func(err error) { t.Errorf("federation runner: %v", err) },
	}
	debounces := make(chan heldTimer, 1)
	runner.SetDebounceTimerForTest(func(time.Duration) Timer {
		timer := heldTimer{fire: make(chan time.Time, 1)}
		debounces <- timer
		return timer
	})
	return runner, debounces
}

func awaitDebounce(t *testing.T, debounces <-chan heldTimer) heldTimer {
	t.Helper()
	select {
	case timer := <-debounces:
		return timer
	case <-time.After(10 * time.Second):
		t.Fatal("the runner did not return to waiting for a wake")
		return heldTimer{}
	}
}

func TestRunnerProjectWakeSyncsOnlyThatProject(t *testing.T) {
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	counter := newCountingHub(t, hub.URL)
	bound := bindSpokeProjects(t, hub.DB, spoke.DB, counter.URL, 2)
	a, b := bound[0], bound[1]

	wake := NewWakeQueue()
	results := newSpokeResults()
	runner, debounces := newWakeRunner(t, spoke, wake, results)
	ctx := t.Context()
	go func() { _ = runner.Run(ctx) }()
	results.await(t, a.spoke.ID, b.spoke.ID)
	start := results.len()
	counter.reset()

	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: a.spoke.ID, Title: "pushed on wake", Author: "tester"})
	require.NoError(t, err)
	wake.WakeProject(a.spoke.ID)
	awaitDebounce(t, debounces).fire <- time.Now()
	results.await(t, a.spoke.ID)
	wake.WakeProject(b.spoke.ID)
	next := awaitDebounce(t, debounces)

	_, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "the woken project pushes its pending event")
	_, byProject := counter.counts()
	assert.Positive(t, byProject[a.hub.ID])
	assert.Zero(t, byProject[b.hub.ID], "a wake for one project does not contact the hub about another")
	assert.Equal(t, []int64{a.spoke.ID}, results.since(start))

	next.fire <- time.Now()
	results.await(t, b.spoke.ID)
	assert.Equal(t, []int64{a.spoke.ID, b.spoke.ID}, results.since(start))
}

func TestRunnerKeepsEveryProjectWakeThatArrivesBeforeAPass(t *testing.T) {
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	bound := bindSpokeProjects(t, hub.DB, spoke.DB, hub.URL, 3)
	a, b, c := bound[0], bound[1], bound[2]

	wake := NewWakeQueue()
	results := newSpokeResults()
	runner, debounces := newWakeRunner(t, spoke, wake, results)
	ctx := t.Context()
	go func() { _ = runner.Run(ctx) }()
	results.await(t, a.spoke.ID, b.spoke.ID, c.spoke.ID)
	start := results.len()

	wake.WakeProject(a.spoke.ID)
	timer := awaitDebounce(t, debounces)
	wake.WakeProject(b.spoke.ID)
	timer.fire <- time.Now()
	results.await(t, a.spoke.ID, b.spoke.ID)
	wake.WakeAll()
	timer = awaitDebounce(t, debounces)
	assert.ElementsMatch(t, []int64{a.spoke.ID, b.spoke.ID}, results.since(start), "both project wakes reach one pass")

	wake.WakeProject(a.spoke.ID)
	timer.fire <- time.Now()
	results.await(t, a.spoke.ID, b.spoke.ID, c.spoke.ID)
	wake.WakeAll()
	awaitDebounce(t, debounces)
	assert.ElementsMatch(t, []int64{a.spoke.ID, b.spoke.ID, a.spoke.ID, b.spoke.ID, c.spoke.ID}, results.since(start),
		"a wake for every spoke absorbs a later project wake")
}

package federation

import (
	"context"
	"errors"
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
	// The hub's held poll can answer the push and give the project another
	// turn, so only the absence of the other project is fixed.
	assert.NotContains(t, results.since(start), b.spoke.ID)

	next.fire <- time.Now()
	results.await(t, b.spoke.ID)
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

// listHookStore runs afterList once, right after a pass lists its bindings,
// and fails that listing when afterList returns an error.
type listHookStore struct {
	db.Storage
	once      sync.Once
	afterList func() error
}

func (s *listHookStore) ListFederationBindings(ctx context.Context) ([]db.FederationBinding, error) {
	bindings, err := s.Storage.ListFederationBindings(ctx)
	var hookErr error
	s.once.Do(func() { hookErr = s.afterList() })
	if hookErr != nil {
		return nil, hookErr
	}
	return bindings, err
}

// A full pass that ends early or misses a wake does not leave a queued wake
// waiting for the next interval.
func TestRunnerKeepsWakesAFullPassDidNotServe(t *testing.T) {
	for _, tc := range []struct {
		name string
		// afterList runs once, after the first pass lists its bindings.
		afterList func(*WakeQueue) error
		// turns is how many turns the project gets without an interval.
		turns int
	}{
		{
			// A write queued before the first pass still syncs soon when that
			// pass fails before it reaches any spoke.
			name:      "listing fails",
			afterList: func(*WakeQueue) error { return errors.New("database unavailable") },
			turns:     1,
		},
		{
			// A wake for every spoke, such as a join, that arrives after the
			// listing gets a pass of its own, because the listing may not
			// include the joined binding.
			name:      "wake arrives after listing",
			afterList: func(wake *WakeQueue) error { wake.WakeAll(); return nil },
			turns:     2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := testenv.New(t)
			spoke := testenv.New(t)
			t.Setenv("KATA_HOME", t.TempDir())
			a := bindSpokeProjects(t, hub.DB, spoke.DB, hub.URL, 1)[0]

			wake := NewWakeQueue()
			wake.WakeProject(a.spoke.ID)
			results := newSpokeResults()
			runner := &Runner{
				DB:            &listHookStore{Storage: spoke.DB, afterList: func() error { return tc.afterList(wake) }},
				Interval:      time.Hour,
				Wake:          wake,
				Debounce:      time.Millisecond,
				OnSpokeResult: results.record,
			}
			go func() { _ = runner.Run(t.Context()) }()
			for range tc.turns {
				results.await(t, a.spoke.ID)
			}
		})
	}
}

func TestRunnerWakePushesMoveIntoLowerIDProjectBeforeTheInterval(t *testing.T) {
	hub := testenv.New(t)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	bound := bindSpokeProjects(t, hub.DB, spoke.DB, hub.URL, 2)
	// The runner serves bindings in project ID order, so the destination
	// takes its turn before the source.
	destination, source := bound[0], bound[1]
	require.Less(t, destination.spoke.ID, source.spoke.ID)

	wake := NewWakeQueue()
	results := newSpokeResults()
	runner, debounces := newWakeRunner(t, spoke, wake, results)
	ctx := t.Context()
	go func() { _ = runner.Run(ctx) }()
	results.await(t, destination.spoke.ID, source.spoke.ID)

	issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.spoke.ID, Title: "moved before its first push", Author: "tester"})
	require.NoError(t, err)
	_, err = spoke.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.spoke.ID, ToProjectID: destination.spoke.ID,
		IfMatchRev: issue.Revision, Actor: "tester",
	})
	require.NoError(t, err)
	// The daemon wakes the source for the creation, and both projects for the move.
	wake.WakeProject(source.spoke.ID)
	wake.WakeProject(destination.spoke.ID)
	awaitDebounce(t, debounces).fire <- time.Now()
	results.await(t, destination.spoke.ID, source.spoke.ID)
	// The runner debounces this wake only after the pass ends.
	wake.WakeAll()
	awaitDebounce(t, debounces)

	got, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "the hub has the issue before the next interval")
	assert.Equal(t, destination.hub.ID, got.ProjectID, "the hub has the move before the next interval")
}

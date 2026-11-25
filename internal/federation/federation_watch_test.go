package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/federationcoord"
)

// awaitTurns waits for the runner to finish a pass for each project in ids, in
// any order.
func awaitTurns(t *testing.T, turns <-chan int64, ids ...int64) {
	t.Helper()
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for len(want) > 0 {
		select {
		case id := <-turns:
			delete(want, id)
		case <-time.After(10 * time.Second):
			t.Fatalf("passes for projects %v did not finish", want)
		}
	}
}

// A spoke bound to several projects on one hub holds one watch for all of
// them, and a commit in one project runs only that project.
func TestFederationRunnerHoldsOneWatchForEveryProjectOnAHub(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	other, otherHub := h.addBinding(t, "other-hub-project", h.url)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	awaitTurns(t, turns, h.spokeProject.ID, other.ID)
	//nolint:kennlint // sleeptest: the watch reaches the hub over real HTTP.
	require.Eventually(t, func() bool { return h.watchRequests.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
		"the spoke never sent a watch")
	//nolint:kennlint // sleeptest: lets both watchers settle into one watch over real HTTP.
	time.Sleep(300 * time.Millisecond)
	h.resetPeakHeld()
	//nolint:kennlint // sleeptest: observes the requests held open over real HTTP.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 1, h.peakHeldRequests(), "the spoke held more than one request for both projects")

	h.createIssueIn(t, otherHub.ID, "Committed while the spoke watches")
	select {
	case id := <-turns:
		require.Equal(t, other.ID, id, "an unrelated project ran after another project's commit")
	case <-time.After(10 * time.Second):
		t.Fatal("the changed project was not synced")
	}
	select {
	case id := <-turns:
		t.Fatalf("project %d ran after another project's commit", id)
	case <-time.After(300 * time.Millisecond): //nolint:kennlint // sleeptest: observes extra turns after a real HTTP wake.
	}
	assert.Zero(t, h.pollsWithWait.Load(), "the spoke held a poll per project although the hub takes watches")
}

// watchNames reports whether a watch request names hubProjectID among its
// supplemental projects, and leaves the body readable for the hub.
func watchNames(t *testing.T, r *http.Request, hubProjectID int64) bool {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		Projects []struct {
			ProjectID int64 `json:"project_id"`
		} `json:"projects"`
	}
	require.NoError(t, json.Unmarshal(raw, &body))
	for _, project := range body.Projects {
		if project.ProjectID == hubProjectID {
			return true
		}
	}
	return false
}

// A project the hub refuses to watch for its token falls back to its own held
// poll, so it still gets a prompt pull.
func TestUnwatchedProjectHoldsItsOwnPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var refused atomic.Bool
	var otherHubID atomic.Int64
	var heldFor atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) && watchNames(t, r, otherHubID.Load()) && refused.CompareAndSwap(false, true) {
			w.Header().Set("X-Kata-Federation-Poll-Wait-Max", "45")
			w.Header().Set("X-Kata-Federation-Watch-Max", "256")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": []int64{}, "unwatched": []int64{otherHubID.Load()}})
			return
		}
		if isHeldPoll(r) {
			heldFor.Store(1)
		}
		hub.ServeHTTP(w, r)
	})
	other, otherHub := h.addBinding(t, "other-hub-project", frontURL)
	otherHubID.Store(otherHub.ID)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	awaitTurns(t, turns, h.spokeProject.ID, other.ID)
	//nolint:kennlint // sleeptest: the fallback poll reaches the hub over real HTTP.
	require.Eventually(t, func() bool { return refused.Load() && heldFor.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
		"the unwatched project never held its own poll")

	h.createIssueIn(t, otherHub.ID, "Committed for the unwatched project")
	awaitTurns(t, turns, other.ID)
}

// A hub that advertises watches behind a proxy that refuses the route still
// gets one held poll per project. A 405 refuses the route whatever its body
// says.
func TestRefusedWatchFallsBackToHeldPolls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"bare 401", http.StatusUnauthorized, ""},
		{"coded 405", http.StatusMethodNotAllowed, `{"error":{"code":"method_not_allowed","message":"method not allowed"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRefusedWatchFallsBackToHeldPolls(t, tc.status, tc.body)
		})
	}
}

func testRefusedWatchFallsBackToHeldPolls(t *testing.T, status int, refusal string) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var watches, held atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
			if refusal != "" {
				w.Header().Set("Content-Type", "application/json")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, refusal)
			return
		}
		if isHeldPoll(r) {
			held.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	other, otherHub := h.addBinding(t, "other-hub-project", frontURL)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	awaitTurns(t, turns, h.spokeProject.ID, other.ID)
	//nolint:kennlint // sleeptest: the fallback polls reach the hub over real HTTP.
	require.Eventually(t, func() bool { return watches.Load() > 0 && held.Load() >= 2 }, 10*time.Second, 10*time.Millisecond,
		"the spoke did not fall back to a held poll per project after its watch was refused")
	sent := watches.Load()
	//nolint:kennlint // sleeptest: watches would reach the hub over real HTTP.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, sent, watches.Load(), "the spoke kept sending watches the hub refused")

	h.createIssueIn(t, otherHub.ID, "Committed after the refused watch")
	awaitTurns(t, turns, other.ID)
}

// An endpoint change for one watched project ends the watch at once, and the
// watch does not carry that project's bearer again until the change is done.
func TestRebindEndsAWatchThatCoversTheProject(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	started := make(chan struct{}, 16)
	canceled := make(chan struct{}, 16)
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			// The server notices a client hang-up only after the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			started <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		hub.ServeHTTP(w, r)
	})
	h.addBinding(t, "other-hub-project", frontURL)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch did not start")
	}
	key := federationcoord.Key(h.spokeDB.InstanceUID(), h.spokeProject.ID)
	done := make(chan error, 1)
	go func() {
		release, err := federationcoord.BeginRebind(context.WithoutCancel(t.Context()), key, h.spokeDB, h.spokeProject.ID)
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second): //nolint:kennlint // deadlinetest: rebind must not wait for the whole watch.
		t.Fatal("an endpoint change waited for the watch to end")
	}
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the watch covering the project did not end")
	}
}

// watchMaxWriter rewrites the watch size a hub advertises.
type watchMaxWriter struct {
	http.ResponseWriter
	value string
}

func (w watchMaxWriter) WriteHeader(code int) {
	if w.Header().Get(federationWatchMaxHeader) != "" {
		w.Header().Set(federationWatchMaxHeader, w.value)
	}
	w.ResponseWriter.WriteHeader(code)
}

// A hub whose watch can name only the route project gives the spoke no shared
// watch, so each project holds its own poll.
func TestWatchOfOneProjectHoldsAPollPerProject(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var watches, held atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
		}
		if isHeldPoll(r) {
			held.Add(1)
		}
		hub.ServeHTTP(watchMaxWriter{ResponseWriter: w, value: "1"}, r)
	})
	other, otherHub := h.addBinding(t, "other-hub-project", frontURL)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	awaitTurns(t, turns, h.spokeProject.ID, other.ID)
	//nolint:kennlint // sleeptest: the held polls reach the hub over real HTTP.
	require.Eventually(t, func() bool { return held.Load() >= 2 }, 10*time.Second, 10*time.Millisecond,
		"the spoke did not hold a poll per project at a hub that watches one project at a time")
	assert.Zero(t, watches.Load(), "the spoke sent watches to a hub that watches one project at a time")

	h.createIssueIn(t, otherHub.ID, "Committed at a one-project hub")
	awaitTurns(t, turns, other.ID)
}

// A hub that refuses the watch's own route credential refuses only that
// binding; the other bindings at the hub keep sharing a watch.
func TestRefusedLeadCredentialKeepsTheOthersWatching(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var watches, held atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
		}
		if isHeldPoll(r) {
			held.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	other, otherHub := h.addBinding(t, "other-hub-project", frontURL)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	awaitTurns(t, turns, h.spokeProject.ID, other.ID)
	//nolint:kennlint // sleeptest: the watch reaches the hub over real HTTP.
	require.Eventually(t, func() bool { return watches.Load() > 0 }, 10*time.Second, 10*time.Millisecond, "the spoke never sent a watch")
	//nolint:kennlint // sleeptest: lets both watchers settle into one watch over real HTTP.
	time.Sleep(300 * time.Millisecond)

	ctx := context.WithoutCancel(t.Context())
	enrollments, err := h.hubDB.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	for _, enrollment := range enrollments {
		if enrollment.ProjectID != nil && *enrollment.ProjectID == h.hubProject.ID {
			require.NoError(t, h.hubDB.RevokeFederationEnrollment(ctx, enrollment.ID))
		}
	}
	sent := watches.Load()
	h.createIssueIn(t, otherHub.ID, "Committed after the lead's credential was revoked")
	awaitTurns(t, turns, other.ID)
	//nolint:kennlint // sleeptest: the next watches reach the hub over real HTTP.
	require.Eventually(t, func() bool { return watches.Load() >= sent+2 }, 10*time.Second, 10*time.Millisecond,
		"the other binding did not watch after the lead's credential was refused")
	//nolint:kennlint // sleeptest: held polls would reach the hub over real HTTP.
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, held.Load(), "one refused credential sent every binding at the hub to its own poll")

	h.createIssueIn(t, otherHub.ID, "Committed beside a refused binding")
	awaitTurns(t, turns, other.ID)
}

// When a hub that takes watches refuses to hold one (at its held-poll bound),
// the spoke must stay multiplexed on one watch per interval instead of falling
// back to an unwaited poll per project every interval. Events still arrive, and
// once the hub accepts holds again, held watches resume.
func TestRefusedWatchStaysMultiplexedAndDeliversEvents(t *testing.T) {
	const projects = 12
	h, _ := newQuietSpoke(t, true, projects)
	h.refuseHolds.Store(true)
	var initialDone atomic.Int64
	h.startConfiguredRunner(t, &Runner{
		DB: h.spokeDB, Interval: 200 * time.Millisecond,
		OnSpokeResult: func(int64, error) { initialDone.Add(1) },
	})
	//nolint:kennlint // sleeptest: waits for the first pass across all bindings over real HTTP.
	require.Eventually(t, func() bool { return initialDone.Load() >= projects }, 30*time.Second, 10*time.Millisecond,
		"the first runner pass did not finish")
	// Wait for the initial refused watch to land and settle.
	//nolint:kennlint // sleeptest: the initial watch reaches the hub over real HTTP.
	require.Eventually(t, func() bool { return h.watchRequests.Load() >= 1 }, 10*time.Second, 10*time.Millisecond,
		"the spoke never sent a watch")
	//nolint:kennlint // sleeptest: lets the refused watch's quiet window begin over real HTTP.
	time.Sleep(300 * time.Millisecond)

	unwaitedBefore := h.pollsWithoutWait.Load()
	watchesBefore := h.watchRequests.Load()
	waitWatchesBefore := h.watchRequestsWithWait.Load()
	//nolint:kennlint // sleeptest: measures idle request counts over real HTTP across several intervals.
	time.Sleep(1500 * time.Millisecond)

	unwaitedDelta := h.pollsWithoutWait.Load() - unwaitedBefore
	watchesDelta := h.watchRequests.Load() - watchesBefore
	waitWatchesDelta := h.watchRequestsWithWait.Load() - waitWatchesBefore
	assert.Zero(t, unwaitedDelta,
		"refused watches fell back to unwaited per-project polls (%d polls in 1.5s)", unwaitedDelta)
	assert.GreaterOrEqual(t, watchesDelta, int64(2), "the spoke stopped checking the hub via watches while holds were refused")
	assert.LessOrEqual(t, watchesDelta, int64(12), "the spoke sent too many watches while holds were refused")
	assert.Less(t, waitWatchesDelta, watchesDelta,
		"hold-backoff watches kept asking to hold on every interval instead of sending wait=0 between probes")

	// Events still arrive promptly on the multiplexed watch interval, pulling only the changed project.
	h.createIssueIn(t, h.hubProject.ID, "Committed while holds are refused")
	//nolint:kennlint // sleeptest: delivery crosses real HTTP on the next watch tick.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 1 }, 5*time.Second, 20*time.Millisecond,
		"event was not delivered while holds were refused")

	// Once the hub stops refusing holds, held watches resume and deliver immediately.
	h.refuseHolds.Store(false)
	//nolint:kennlint // sleeptest: waits for the next hold probe to stay open over real HTTP.
	require.Eventually(t, func() bool {
		h.heldMu.Lock()
		defer h.heldMu.Unlock()
		return h.held == 1
	}, 10*time.Second, 20*time.Millisecond, "the spoke did not resume holding a watch once hub capacity returned")
	h.createIssueIn(t, h.hubProject.ID, "Committed after holds recovered")
	//nolint:kennlint // sleeptest: delivery crosses real HTTP.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 2 }, 5*time.Second, 10*time.Millisecond)
}

// Watchers joining over a startup pass that spans more than one join delay
// coalesce instead of cancelling and reissuing a partial watch on every join.
func TestWatchJoinDebounceAvoidsStartupReissueStorm(t *testing.T) {
	const projects = 10
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var watches atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
		}
		if isEventPoll(r) {
			// Spread initial pulls across ~250 ms (> 100 ms join delay) to mirror real hub RTT.
			//nolint:kennlint // sleeptest: simulates network round-trip time on initial pulls.
			time.Sleep(25 * time.Millisecond)
		}
		hub.ServeHTTP(w, r)
	})
	var lastHubID int64
	var lastSpokeID int64
	for i := 1; i < projects; i++ {
		spoke, hub := h.addBinding(t, "debounce-project-"+string(rune('a'+i)), frontURL)
		lastSpokeID, lastHubID = spoke.ID, hub.ID
	}
	turns := make(chan int64, 2*projects)
	h.startConfiguredRunner(t, &Runner{
		DB: h.spokeDB, Interval: time.Hour,
		OnSpokeResult: func(id int64, _ error) { turns <- id },
	})
	for range projects {
		select {
		case <-turns:
		case <-time.After(30 * time.Second):
			t.Fatal("initial pass did not finish")
		}
	}
	//nolint:kennlint // sleeptest: waits for the coalesced watch to reach the hub over real HTTP.
	require.Eventually(t, func() bool { return watches.Load() >= 1 }, 10*time.Second, 10*time.Millisecond)
	//nolint:kennlint // sleeptest: verifies no trailing reissue storm after all watchers joined.
	time.Sleep(400 * time.Millisecond)
	assert.LessOrEqual(t, watches.Load(), int64(2),
		"startup joins triggered %d watches instead of coalescing", watches.Load())

	// The last project to join is still watched and receives commits promptly.
	h.createIssueIn(t, lastHubID, "Committed on the last project to join")
	awaitTurns(t, turns, lastSpokeID)
}

// When a hub throttles a multiplexed watch with 429 or 503, the hub cooldown
// keeps both the runner's interval loop and the watch group quiet until Retry-After.
func TestWatchThrottleDelaysIntervalAndWatchRetries(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var throttle atomic.Bool
	var watches, intervalPolls atomic.Int64
	throttled := make(chan struct{}, 1)
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
			if throttle.Load() {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				select {
				case throttled <- struct{}{}:
				default:
				}
				return
			}
		}
		if isEventPoll(r) {
			intervalPolls.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	const extra = 5
	ids := []int64{h.spokeProject.ID}
	for i := range extra {
		spoke, _ := h.addBinding(t, "throttle-project-"+string(rune('a'+i)), frontURL)
		ids = append(ids, spoke.ID)
	}
	throttle.Store(true)
	turns := make(chan int64, 2*len(ids))
	retries := NewSyncRetries()
	h.startConfiguredRunner(t, &Runner{
		DB: h.spokeDB, Interval: 200 * time.Millisecond, Retries: retries,
		OnSpokeResult: func(id int64, _ error) { turns <- id },
	})
	awaitTurns(t, turns, ids...)
	select {
	case <-throttled:
	case <-time.After(10 * time.Second):
		t.Fatal("throttled watch did not reach the hub")
	}
	// Give the spoke a moment to record the hub cooldown.
	//nolint:kennlint // sleeptest: waits for the 429 response to be processed over real HTTP.
	time.Sleep(100 * time.Millisecond)
	watchesAfter := watches.Load()
	pollsAfter := intervalPolls.Load()
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: observes several intervals and a wake while Retry-After is active.
	time.Sleep(time.Second)
	assert.Equal(t, pollsAfter, intervalPolls.Load(), "runner polled bindings at a hub still in Retry-After cooldown")
	assert.Equal(t, watchesAfter, watches.Load(), "watch group retried before Retry-After elapsed")
}

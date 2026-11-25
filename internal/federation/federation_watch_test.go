package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
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

	// While holds are refused, no watch stays held on the hub.
	h.heldMu.Lock()
	assert.Zero(t, h.held, "refused holds must not leave watches open")
	h.heldMu.Unlock()

	// Once the hub stops refusing holds, a held watch stays open and wakes immediately on commit.
	h.refuseHolds.Store(false)
	//nolint:kennlint // sleeptest: waits for the next hold probe to stay open over real HTTP.
	require.Eventually(t, func() bool {
		h.heldMu.Lock()
		defer h.heldMu.Unlock()
		return h.held == 1
	}, 10*time.Second, 20*time.Millisecond, "the spoke did not resume holding a watch once hub capacity returned")
	unwaitedBeforeCommit := h.watchRequestsWithoutWait.Load()
	h.createIssueIn(t, h.hubProject.ID, "Committed after holds recovered")
	//nolint:kennlint // sleeptest: delivery crosses real HTTP.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, unwaitedBeforeCommit, h.watchRequestsWithoutWait.Load(),
		"commit after hold recovery should wake the held watch rather than an unwaited fallback poll")
}

// Watchers joining over a startup burst that spans more than one 100 ms join
// delay coalesce into a single watch instead of cancelling and reissuing a
// partial watch on every arrival.
func TestWatchJoinDebounceAvoidsStartupReissueStorm(t *testing.T) {
	const projects = 10
	var watches, cancelled atomic.Int64
	gotWatch := make(chan int, projects)
	releaseWatch := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		watches.Add(1)
		var body struct {
			Projects []json.RawMessage `json:"projects"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case gotWatch <- len(body.Projects) + 1:
		default:
		}
		select {
		case <-releaseWatch:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Kata-Federation-Poll-Wait-Max", "45")
			w.Header().Set("X-Kata-Federation-Watch-Max", "256")
			_, _ = w.Write([]byte(`{"ready":[],"unwatched":[]}`))
		case <-r.Context().Done():
			cancelled.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	polls := newHubLongPolls(t.Context(), nil, nil, client.Opts{AllowInsecure: true, Timeout: 30 * time.Second}, 30*time.Second, nil, nil, NewSyncRetries(), time.Now)
	t.Cleanup(polls.stopAll)
	polls.learnHub(srv.URL, hubPollAdvertisement{wait: 45 * time.Second, watchMax: 256})
	leadClient, err := NewClient(t.Context(), srv.URL, "token", client.Opts{AllowInsecure: true, Timeout: 30 * time.Second})
	require.NoError(t, err)
	group := polls.watchGroup(leadClient, config.FederationCredential{HubURL: srv.URL, Token: "token", AllowInsecure: true})
	require.NotNil(t, group)

	var wg sync.WaitGroup
	results := make(chan heldResult, projects)
	for i := range projects {
		c, err := NewClient(t.Context(), srv.URL, "token", client.Opts{AllowInsecure: true, Timeout: 30 * time.Second})
		require.NoError(t, err)
		member := &watchMember{
			projectID:    int64(i + 1),
			hubProjectID: int64(100 + i),
			token:        "token",
			client:       c,
			wait:         45 * time.Second,
			finishSync:   func() {},
		}
		wg.Go(func() {
			results <- group.hold(t.Context(), member)
		})
		if i+1 < projects {
			// Spread 10 joins across ~180 ms (> 100 ms join delay, < 500 ms cap).
			//nolint:kennlint // sleeptest: exercises sliding join debounce across real timers.
			time.Sleep(20 * time.Millisecond)
		}
	}

	select {
	case count := <-gotWatch:
		assert.Equal(t, projects, count, "all staggered joins should coalesce into one watch")
	case <-time.After(5 * time.Second):
		t.Fatal("coalesced watch did not reach the hub")
	}
	close(releaseWatch)
	wg.Wait()
	assert.Equal(t, int64(1), watches.Load(), "staggered startup joins must send one watch")
	assert.Zero(t, cancelled.Load(), "staggered startup joins must not cancel and reissue partial watches")
}

// When a hub throttles a multiplexed watch with 429 or 503, the hub cooldown
// also stops unrelated bindings on the same hub that were NOT part of that
// throttled watch (and therefore carry no per-project retry schedule).
func TestWatchThrottleDelaysIntervalAndWatchRetries(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var throttle atomic.Bool
	var watches, intervalPolls atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
			if throttle.Load() {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		if isEventPoll(r) {
			intervalPolls.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	const extra = 3
	ids := []int64{h.spokeProject.ID}
	for i := range extra {
		spoke, _ := h.addBinding(t, "throttle-project-"+string(rune('a'+i)), frontURL)
		ids = append(ids, spoke.ID)
	}
	throttle.Store(true)
	turns := make(chan int64, 64)
	retries := NewSyncRetries()
	h.startConfiguredRunner(t, &Runner{
		DB: h.spokeDB, Interval: 200 * time.Millisecond, Retries: retries,
		OnSpokeResult: func(id int64, _ error) {
			select {
			case turns <- id:
			default:
			}
		},
	})
	awaitTurns(t, turns, ids...)
	// Wait until the hub-wide cooldown is recorded from the throttled watch.
	//nolint:kennlint // sleeptest: waits for the 429 response to be processed over real HTTP.
	require.Eventually(t, func() bool {
		return retries.hubBlocked(frontURL, time.Now())
	}, 10*time.Second, 10*time.Millisecond, "throttled watch did not record hub cooldown")

	// Add a brand-new binding on the SAME hub after the 429; it was never in
	// the throttled watch and has no per-project retry schedule.
	freshSpoke, _ := h.addBinding(t, "throttle-fresh-project", frontURL)
	_, hasPerProjectRetry := retries.FederationSyncRetry(freshSpoke.ID)
	require.False(t, hasPerProjectRetry, "fresh binding must have no per-project retry schedule")

	watchesAfter := watches.Load()
	pollsAfter := intervalPolls.Load()
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: observes several intervals and a wake while Retry-After is active.
	time.Sleep(800 * time.Millisecond)
	assert.Equal(t, pollsAfter, intervalPolls.Load(), "hub cooldown must stop polls even for a fresh binding on the same hub")
	assert.Equal(t, watchesAfter, watches.Load(), "watch group retried before Retry-After elapsed")
}

// Once a hub cooldown expires, the watch group sends a wait=0 recovery probe
// so a healthy idle hub clears the cooldown immediately instead of holding the
// recovery probe open for 45 seconds, then resumes held watches.
func TestWatchRecoveryProbeUsesZeroWaitAndResumesHeldWatch(t *testing.T) {
	var seenWaits []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenWaits = append(seenWaits, r.URL.Query().Get("wait"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kata-Federation-Poll-Wait-Max", "45")
		w.Header().Set("X-Kata-Federation-Watch-Max", "256")
		_, _ = w.Write([]byte(`{"ready":[],"unwatched":[]}`))
	}))
	t.Cleanup(srv.Close)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retries := newSyncRetries(noJitter)
	retries.recordHubUnavailable(srv.URL, 0, time.Minute, 30*time.Second, now)
	now = now.Add(time.Minute) // Cooldown has just expired.

	polls := newHubLongPolls(t.Context(), nil, nil, client.Opts{AllowInsecure: true, Timeout: 30 * time.Second}, 30*time.Second, nil, nil, retries, func() time.Time { return now })
	t.Cleanup(polls.stopAll)
	polls.learnHub(srv.URL, hubPollAdvertisement{wait: 45 * time.Second, watchMax: 256})
	c, err := NewClient(t.Context(), srv.URL, "token", client.Opts{AllowInsecure: true, Timeout: 30 * time.Second})
	require.NoError(t, err)
	group := polls.watchGroup(c, config.FederationCredential{HubURL: srv.URL, Token: "token", AllowInsecure: true})
	require.NotNil(t, group)
	member := &watchMember{
		projectID: 1, hubProjectID: 100, token: "token", client: c, wait: 45 * time.Second, finishSync: func() {},
	}
	res := group.hold(t.Context(), member)
	require.NoError(t, res.err)
	require.True(t, res.again, "clean wait=0 recovery probe should ask member to immediately resume held watch")
	require.False(t, retries.hubBlocked(srv.URL, now), "recovery probe must clear hub cooldown")

	// Next join after cooldown clears must request wait=45 again.
	res2 := group.hold(t.Context(), member)
	require.NoError(t, res2.err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"0", "45"}, seenWaits, "first post-cooldown watch must be wait=0 probe, followed by held wait=45")
}

// Joining a watch group during refused-hold pacing returns the remaining wait
// immediately so the watcher sleeps outside the project's endpoint gate and
// concurrent rebinds do not stall for the full sync interval.
func TestWatchPacingWaitReturnsImmediatelyToCaller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kata-Federation-Poll-Wait-Max", "45")
		w.Header().Set("X-Kata-Federation-Watch-Max", "256")
		_, _ = w.Write([]byte(`{"ready":[],"unwatched":[]}`))
	}))
	t.Cleanup(srv.Close)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	polls := newHubLongPolls(t.Context(), nil, nil, client.Opts{AllowInsecure: true, Timeout: 30 * time.Second}, 30*time.Second, nil, nil, newSyncRetries(noJitter), func() time.Time { return now })
	polls.jitter = noJitter
	t.Cleanup(polls.stopAll)
	polls.learnHub(srv.URL, hubPollAdvertisement{wait: 45 * time.Second, watchMax: 256})
	c, err := NewClient(t.Context(), srv.URL, "token", client.Opts{AllowInsecure: true, Timeout: 30 * time.Second})
	require.NoError(t, err)
	group := polls.watchGroup(c, config.FederationCredential{HubURL: srv.URL, Token: "token", AllowInsecure: true})
	require.NotNil(t, group)
	member := &watchMember{
		projectID: 1, hubProjectID: 100, token: "token", client: c, wait: 45 * time.Second, finishSync: func() {},
	}
	// First watch returns immediately (refused hold), setting nextPollAt = now + 30s.
	first := group.hold(t.Context(), member)
	require.NoError(t, first.err)
	require.True(t, first.quiet)
	require.Equal(t, 30*time.Second, first.sleep)

	// A second member joining while nextPollAt is 30s away must get an immediate
	// pace-sleep reply instead of parking inside the group holding its endpoint gate.
	start := time.Now()
	second := group.hold(t.Context(), member)
	require.Less(t, time.Since(start), time.Second, "pacing join must not block caller inside watch group")
	require.NoError(t, second.err)
	require.True(t, second.again)
	require.Equal(t, 30*time.Second, second.sleep)
}

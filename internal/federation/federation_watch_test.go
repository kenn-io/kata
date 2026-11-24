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

	h.createIssueIn(t, otherHub.ID, "Committed while the spoke watches")
	awaitTurns(t, turns, other.ID)
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
// gets one held poll per project.
func TestRefusedWatchFallsBackToHeldPolls(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.watches.Store(true)
	var watches, held atomic.Int64
	frontURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isWatch(r) {
			watches.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
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

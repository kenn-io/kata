package federation

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationRunnerAppliesHubEventsBeforeItsNextInterval(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.startRunner(t, time.Hour)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(200 * time.Millisecond)

	h.createHubIssue(t, "Committed while the spoke waits")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond, "the spoke waited for its interval instead of the held poll")
}

func TestFederationRunnerSkipsIntervalPullsWhileTheHubHoldsItsPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.startRunner(t, 200*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")
	// Let the first held poll prove that the hub holds it.
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(time.Second)
	before := h.pollsWithoutWait.Load()
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(2 * time.Second)
	assert.Equal(t, before, h.pollsWithoutWait.Load(), "interval passes polled a hub that was already holding a poll")
	assert.LessOrEqual(t, h.pollsWithWait.Load(), int64(3))
}

func TestFederationRunnerKeepsIntervalPullsForHubsWithoutLongPoll(t *testing.T) {
	h := newHubSpokeHarness(t, true)
	h.startRunner(t, 200*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(2 * time.Second)
	assert.GreaterOrEqual(t, h.pollsWithoutWait.Load(), int64(5), "a spoke must keep its interval when the hub cannot hold polls")
	assert.LessOrEqual(t, h.pollsWithWait.Load(), int64(1), "a spoke must stop asking a hub that does not advertise long polls")

	h.createHubIssue(t, "Delivered on the interval")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond)
}

// bindThrough points the spoke's binding at a front server that sends each
// request to serve along with a proxy to the hub, and returns its URL.
func (h *hubSpokeHarness) bindThrough(t *testing.T, serve func(http.ResponseWriter, *http.Request, http.Handler)) string {
	t.Helper()
	target, err := url.Parse(h.hubURL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, proxy) }))
	t.Cleanup(front.Close)
	ctx := context.WithoutCancel(t.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(t, err)
	binding.HubURL = front.URL
	_, err = h.spokeDB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	return front.URL
}

func isHeldPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Has("wait")
}

func isEventPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") &&
		!r.URL.Query().Has("wait") && r.URL.Query().Get("issue_uid") == ""
}

// A pass that keeps failing, such as one stopped by a push quarantine, must
// not turn the held poll into a loop: each answer with work would start a pass
// that fails before it pulls, and the next poll would answer at once.
func TestFederationRunnerStopsHoldingPollsWhileItsPassesFail(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	var held atomic.Int64
	var failPulls atomic.Bool
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
		}
		if failPulls.Load() && isEventPoll(r) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, time.Hour)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")

	failPulls.Store(true)
	before := held.Load()
	h.createHubIssue(t, "Waiting behind a failing pass")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(time.Second)
	assert.Equal(t, before, held.Load(), "the spoke kept holding polls while every pass failed")
	assert.Zero(t, h.spokeIssueCount(t))
}

// Moving a binding to a new hub address must not leave interval passes
// skipping the pull because a poll is still held at the old address.
func TestFederationRunnerPullsFromAMovedHubWhileTheOldAddressHoldsItsPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	ctx := context.WithoutCancel(t.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(t, err)
	movedTo := binding.HubURL
	var held atomic.Int64
	// The old address holds polls until the spoke gives up on them and never
	// answers with the hub's new events.
	oldAddress := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			<-r.Context().Done()
			return
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, 200*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the old address")

	_, err = h.spokeDB.RebindFederationBinding(ctx, db.RebindFederationBindingParams{
		ProjectID: h.spokeProject.ID, ExpectedHubURL: oldAddress,
		HubProjectID: binding.HubProjectID, HubProjectUID: binding.HubProjectUID, TargetHubURL: movedTo,
	})
	require.NoError(t, err)
	h.createHubIssue(t, "Committed after the move")
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP on its interval.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond, "interval passes skipped the moved hub while the old address held a poll")
}

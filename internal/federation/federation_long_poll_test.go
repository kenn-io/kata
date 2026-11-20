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
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
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

// front starts a server that sends each request to serve along with a proxy
// to the hub, and returns its URL.
func (h *hubSpokeHarness) front(t *testing.T, serve func(http.ResponseWriter, *http.Request, http.Handler)) string {
	t.Helper()
	target, err := url.Parse(h.hubURL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serve(w, r, proxy) }))
	t.Cleanup(front.Close)
	return front.URL
}

// bindThrough points the spoke's binding and credential at a front server, as
// a join through that address would, and returns its URL.
func (h *hubSpokeHarness) bindThrough(t *testing.T, serve func(http.ResponseWriter, *http.Request, http.Handler)) string {
	t.Helper()
	frontURL := h.front(t, serve)
	ctx := context.WithoutCancel(t.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(t, err)
	binding.HubURL = frontURL
	_, err = h.spokeDB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	h.setCredentialHub(t, frontURL)
	return frontURL
}

// setCredentialHub rewrites the hub URL the spoke's stored credential names.
func (h *hubSpokeHarness) setCredentialHub(t *testing.T, hubURL string) {
	t.Helper()
	credential, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), h.spokeProject.UID)
	require.NoError(t, err)
	require.True(t, found)
	credential.HubURL = hubURL
	require.NoError(t, config.WriteFederationCredential(h.spokeProject.UID, credential))
}

// rebindTo moves the spoke to a new hub address the way a completed rebind
// does: the credential first, then the binding.
func (h *hubSpokeHarness) rebindTo(t *testing.T, hubURL string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(t, err)
	h.setCredentialHub(t, hubURL)
	_, err = h.spokeDB.RebindFederationBinding(ctx, db.RebindFederationBindingParams{
		ProjectID: h.spokeProject.ID, ExpectedHubURL: binding.HubURL,
		HubProjectID: binding.HubProjectID, HubProjectUID: binding.HubProjectUID, TargetHubURL: hubURL,
	})
	require.NoError(t, err)
}

// lastPullSuccess reads when the spoke last recorded a completed pull.
func (h *hubSpokeHarness) lastPullSuccess(t *testing.T) time.Time {
	t.Helper()
	status, err := h.spokeDB.FederationSyncStatusByProject(context.WithoutCancel(t.Context()), h.spokeProject.ID)
	require.NoError(t, err)
	require.NotNil(t, status.LastPullSuccessAt)
	return *status.LastPullSuccessAt
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
	var held, failed atomic.Int64
	var failPulls atomic.Bool
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
		}
		if failPulls.Load() && isEventPoll(r) {
			failed.Add(1)
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
	// The held poll's answer starts a pass, and that pass fails its pull.
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return failed.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the held poll's answer never started a pass")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(time.Second)
	assert.Equal(t, before, held.Load(), "the spoke kept holding polls while every pass failed")
}

// Moving a binding to a new hub address must not leave interval passes
// skipping the pull because a poll is still held at the old address.
func TestFederationRunnerPullsFromAMovedHubWhileTheOldAddressHoldsItsPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	binding, err := h.spokeDB.FederationBindingByProject(context.WithoutCancel(t.Context()), h.spokeProject.ID)
	require.NoError(t, err)
	movedTo := binding.HubURL
	var held atomic.Int64
	// The old address holds polls until the spoke gives up on them and never
	// answers with the hub's new events.
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
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

	h.rebindTo(t, movedTo)
	h.createHubIssue(t, "Committed after the move")
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP on its interval.
	require.Eventually(t, func() bool { return h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond, "interval passes skipped the moved hub while the old address held a poll")
}

// The watcher reads its binding and credential under the same admission as a
// pass, so an endpoint change cannot hand it one side of the change with the
// other: it waits the change out instead.
func TestHubLongPollWaitsOutAnEndpointChange(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	binding, err := h.spokeDB.FederationBindingByProject(t.Context(), h.spokeProject.ID)
	require.NoError(t, err)
	polls := newHubLongPolls(t.Context(), h.spokeDB, config.DefaultFederationCredentialStore(),
		(&Runner{}).clientOpts(), time.Hour, nil)
	t.Cleanup(polls.stopAll)

	finish, err := federationcoord.BeginRebind(t.Context(),
		federationcoord.Key(h.spokeDB.InstanceUID(), h.spokeProject.ID), h.spokeDB, h.spokeProject.ID)
	require.NoError(t, err)
	_, _, ok := polls.pollTarget(t.Context(), h.spokeProject.ID)
	assert.False(t, ok, "the watcher read its binding and credential during an endpoint change")

	finish()
	_, credential, ok := polls.pollTarget(t.Context(), h.spokeProject.ID)
	require.True(t, ok, "the watcher stayed blocked after the endpoint change finished")
	assert.Equal(t, binding.HubURL, credential.HubURL)
}

// Interval passes that skip their pull while a poll is held must not report a
// pull that never happened: a hub that stops answering would look healthy.
func TestFederationRunnerRecordsNoPullWhileAHeldPollGoesUnanswered(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	var held atomic.Int64
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			<-r.Context().Done()
			return
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, 100*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")
	before := h.lastPullSuccess(t)
	//nolint:kennlint // sleeptest: the runner's interval passes run on real time.
	time.Sleep(time.Second)
	assert.Equal(t, before, h.lastPullSuccess(t), "a pass that skipped its pull recorded a pull success")
}

// A hub that answers a held poll with an empty page has answered the pull the
// interval passes skip, so the spoke records that pull as done.
func TestFederationRunnerRecordsAPullWhenTheHubAnswersAHeldPollEmpty(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	var held atomic.Int64
	answer := make(chan struct{})
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			select {
			case <-r.Context().Done():
				return
			case <-answer:
			}
			// Answer at once with the hub's empty page.
			query := r.URL.Query()
			query.Del("wait")
			r.URL.RawQuery = query.Encode()
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, time.Hour)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")
	before := h.lastPullSuccess(t)
	close(answer)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.lastPullSuccess(t).After(before) },
		5*time.Second, 20*time.Millisecond, "the hub's empty answer was not recorded as a pull")
}

// A watcher waiting to reprobe a hub without held polls must not keep waiting
// after the binding moves to a hub that has them.
func TestFederationRunnerHoldsPollsAtAMovedHubThatSupportsThem(t *testing.T) {
	h := newHubSpokeHarness(t, true)
	var held atomic.Int64
	// Started before the runner, so the runner stops before this server closes.
	movedTo := h.front(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, 100*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never probed the hub for held polls")
	//nolint:kennlint // sleeptest: lets the probe's answer reach the watcher over real HTTP.
	time.Sleep(200 * time.Millisecond)

	h.rebindTo(t, movedTo)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		5*time.Second, 20*time.Millisecond, "the spoke kept waiting to reprobe the old address")
}

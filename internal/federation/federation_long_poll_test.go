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
	clientpkg "go.kenn.io/kata/internal/client"
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
		(&Runner{}).clientOpts(), time.Hour, nil, nil)
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

// startWatcher holds polls for the harness binding with no runner. A caller
// that wants runner passes drains polls.changed itself.
func (h *hubSpokeHarness) startWatcher(tb testing.TB, interval time.Duration) *hubLongPolls {
	tb.Helper()
	ctx := context.WithoutCancel(tb.Context())
	polls := newHubLongPolls(ctx, h.spokeDB, config.DefaultFederationCredentialStore(),
		clientOptsWithDefault(clientpkg.Opts{}), interval, nil, nil)
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(tb, err)
	polls.start(activeSpokeBinding{binding: binding, project: h.spokeProject})
	tb.Cleanup(polls.stopAll)
	return polls
}

func (h *hubSpokeHarness) catchUp(tb testing.TB) {
	tb.Helper()
	require.NoError(tb, (&Runner{DB: h.spokeDB}).RunOnce(context.WithoutCancel(tb.Context())))
}

// An endpoint change drains the project's transport work before it switches
// the hub address. A held poll reads its address and sends its bearer inside
// that same project gate, so the bearer never reaches the old address after
// the change; the gate is released once the request is sent, so the change
// does not wait for the hold.
func TestHeldPollSendsItsBearerInsideTheEndpointGate(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.catchUp(t)
	key := federationcoord.Key(h.spokeDB.InstanceUID(), h.spokeProject.ID)
	finishRebind, err := federationcoord.BeginRebind(t.Context(), key, h.spokeDB, h.spokeProject.ID)
	require.NoError(t, err)
	h.startWatcher(t, time.Hour)
	//nolint:kennlint // sleeptest: gives a watcher that ignores the gate time to reach the old hub over real HTTP.
	time.Sleep(300 * time.Millisecond)
	moved := h.newFront(t)
	h.moveTo(t, moved)
	finishRebind()
	//nolint:kennlint // sleeptest: the watcher holds its poll over real HTTP.
	require.Eventually(t, func() bool { return moved.pollsWithWait.Load() == 1 },
		10*time.Second, 10*time.Millisecond, "the watcher never polled the new address")
	assert.Zero(t, h.pollsWithWait.Load(), "the watcher sent its bearer to the old address during the change")

	done := make(chan struct{})
	go func() {
		defer close(done)
		finish, err := federationcoord.BeginRebind(context.WithoutCancel(t.Context()), key, h.spokeDB, h.spokeProject.ID)
		if err == nil {
			finish()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second): //nolint:kennlint // deadlinetest: a rebind that waits for the whole 45-second hold fails here.
		t.Fatal("an endpoint change waited for the held poll to end")
	}
}

// After an endpoint change, the poll held at the old address no longer
// stands for the binding, so the runner pulls from the new one at once.
func TestEndpointChangeEndsTheSkipForTheOldHeldPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.startRunner(t, time.Hour)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 10*time.Millisecond, "the spoke never held a poll at the hub")
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(200 * time.Millisecond)
	moved := h.newFront(t)
	h.moveTo(t, moved)
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP.
	require.Eventually(t, func() bool { return moved.pollsWithoutWait.Load()+moved.pollsWithWait.Load() > 0 },
		5*time.Second, 10*time.Millisecond, "the runner kept skipping pulls for a poll held at the old address")
}

// The last successful pull time means the hub answered. A held poll that is
// still unanswered, as after a network interruption, must not advance it.
func TestUnansweredHeldPollRecordsNoPullSuccess(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.holdForever.Store(true)
	h.startRunner(t, 100*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 10*time.Millisecond, "the spoke never held a poll at the hub")
	//nolint:kennlint // sleeptest: lets an interval pass that skips its pull finish.
	time.Sleep(200 * time.Millisecond)
	before := h.lastPullSuccess(t)
	//nolint:kennlint // sleeptest: several interval passes run while the poll stays unanswered.
	time.Sleep(time.Second)
	assert.Equal(t, before, h.lastPullSuccess(t), "an interval pass recorded a pull the hub never answered")
}

// A held poll the hub answers with nothing new is a successful pull.
func TestAnsweredHeldPollRecordsAPullSuccess(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.holdSeconds.Store(1)
	h.startRunner(t, time.Hour)
	h.waitForFirstPull(t)
	before := h.lastPullSuccess(t)
	//nolint:kennlint // sleeptest: the hub answers a one-second hold over real HTTP.
	require.Eventually(t, func() bool { return h.lastPullSuccess(t).After(before) },
		10*time.Second, 20*time.Millisecond, "an answered held poll did not record its pull")
}

// A pass that keeps failing before its pull leaves the hub's page unread. The
// watcher must not re-poll and re-trigger a pass at network speed.
func TestWatcherBacksOffWhenPassesLeaveItsPageUnread(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	polls := h.startWatcher(t, 100*time.Millisecond)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-polls.changed:
				// A pass that fails before it pulls: the cursor stays put.
				polls.passCompleted()
			}
		}
	}()
	//nolint:kennlint // sleeptest: counts the polls a watcher sends over a fixed real-time window.
	time.Sleep(1500 * time.Millisecond)
	assert.LessOrEqual(t, h.pollsWithWait.Load(), int64(6), "the watcher re-polled without backing off")
}

// A hub at its held-poll bound answers at once. While the runner's interval
// pull covers the binding, the watcher's own retries back off.
func TestRefusedHoldsBackOff(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.refuseHolds.Store(true)
	h.catchUp(t)
	h.startWatcher(t, 100*time.Millisecond)
	//nolint:kennlint // sleeptest: counts the polls a watcher sends over a fixed real-time window.
	time.Sleep(1500 * time.Millisecond)
	assert.LessOrEqual(t, h.pollsWithWait.Load(), int64(5), "a refused hold was retried every interval")
}

// A request timeout too short for held polls turns them off. The runner says
// so once, instead of leaving the operator to infer it from the docs.
func TestShortRequestTimeoutReportsHeldPollsOff(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	notices := make(chan error, 8)
	h.startConfiguredRunner(t, &Runner{
		DB: h.spokeDB, Interval: time.Hour, Opts: clientpkg.Opts{Timeout: 5 * time.Second},
		OnError: func(err error) { notices <- err },
	})
	select {
	case err := <-notices:
		assert.Contains(t, err.Error(), "held federation polls off")
	case <-time.After(10 * time.Second): //nolint:kennlint // deadlinetest: bounds the wait for the runner's notice.
		t.Fatal("the runner never reported that held polls are off")
	}
	assert.Zero(t, h.pollsWithWait.Load())
}

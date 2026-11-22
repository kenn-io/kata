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
	"testing/synctest"
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
		(&Runner{}).clientOpts(), time.Hour, nil, nil, NewSyncRetries(), time.Now)
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
		clientOptsWithDefault(clientpkg.Opts{}), interval, nil, nil, NewSyncRetries(), time.Now)
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
	ctx := context.WithoutCancel(t.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(t, err)
	moved := h.newFront(t)
	movedTo := moved.url
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
	require.NoError(t, config.WriteFederationCredential(h.spokeProject.UID, config.FederationCredential{HubURL: movedTo, HubProjectID: h.hubProject.ID, Token: h.pullToken}))
	h.createHubIssue(t, "Committed after the move")
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP on its interval.
	require.Eventually(t, func() bool { return moved.pollsWithoutWait.Load() > 0 && h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond, "interval passes skipped the moved hub while the old address held a poll")
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

// allowPush gives the spoke a pull and push enrollment, so its passes push
// local work to the hub.
func (h *hubSpokeHarness) allowPush(tb testing.TB) {
	tb.Helper()
	ctx := context.WithoutCancel(tb.Context())
	created, err := h.hubDB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "delivery-push-token", SpokeInstanceUID: h.spokeDB.InstanceUID(),
		ProjectID: &h.hubProject.ID, Capabilities: "pull,push", Actor: "tester",
	})
	require.NoError(tb, err)
	h.pullToken = created.Token
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(tb, err)
	binding.PushEnabled, binding.Actor = true, "tester"
	_, err = h.spokeDB.UpsertFederationBinding(ctx, binding)
	require.NoError(tb, err)
	require.NoError(tb, config.WriteFederationCredential(h.spokeProject.UID, config.FederationCredential{
		HubURL: binding.HubURL, HubProjectID: h.hubProject.ID, Token: h.pullToken,
	}))
}

// A hub that throttles the binding's pass with Retry-After must not see a held
// poll from the spoke before the delay it asked for ends. The held poll itself
// stays caught up, so only the retry schedule holds the watcher back.
func TestThrottledBindingHoldsNoPollBeforeItsRetry(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.allowPush(t)
	var held, open atomic.Int64
	var throttle atomic.Bool
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			open.Add(1)
			defer open.Add(-1)
		}
		if throttle.Load() && !isHeldPoll(r) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		hub.ServeHTTP(w, r)
	})
	retries := NewSyncRetries()
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, Retries: retries})
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return open.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")

	throttle.Store(true)
	_, _, err := h.spokeDB.CreateIssue(context.WithoutCancel(t.Context()), db.CreateIssueParams{
		ProjectID: h.spokeProject.ID, Title: "Waiting on the hub's retry", Author: "worker",
	})
	require.NoError(t, err)
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: the runner pushes over real HTTP after the wake.
	require.Eventually(t, func() bool {
		_, scheduled := retries.FederationSyncRetry(h.spokeProject.ID)
		return scheduled && open.Load() == 0
	}, 10*time.Second, 20*time.Millisecond, "the spoke kept its held poll open after the hub asked it to wait")
	before := held.Load()
	throttle.Store(false)
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: give the runner time to handle the wake.
	time.Sleep(time.Second)
	assert.Equal(t, before, held.Load(), "the spoke held a poll before the hub's Retry-After ended")
}

func TestHeldPollDoesNotFollowRedirect(t *testing.T) {
	var redirected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(t.Context(), server.URL, "token", clientOptsWithDefault(clientpkg.Opts{}))
	require.NoError(t, err)
	_, _, err = client.pollProjectEvents(t.Context(), 1, "", 0, 1, time.Second)
	require.Error(t, err)
	require.Zero(t, redirected.Load())
}

func TestHeldPollRetryAfterBlocksRunner(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	var held, ordinary atomic.Int64
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if isEventPoll(r) {
			ordinary.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	scheduled := make(chan struct{}, 1)
	retries := newSyncRetries(func(time.Duration) time.Duration {
		select {
		case scheduled <- struct{}{}:
		default:
		}
		return 0
	})
	stop := h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: 100 * time.Millisecond, Retries: retries})
	select {
	case <-scheduled:
	case <-time.After(10 * time.Second):
		t.Fatal("the held poll did not record a retry")
	}
	_, ok := retries.FederationSyncRetry(h.spokeProject.ID)
	require.True(t, ok)
	beforeHeld, beforeOrdinary := held.Load(), ordinary.Load()
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: observe real HTTP requests across several runner intervals.
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, beforeHeld, held.Load())
	require.Equal(t, beforeOrdinary, ordinary.Load())

	var newHeld atomic.Int64
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			newHeld.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	t.Cleanup(stop)
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: the runner resumes HTTP traffic at the changed endpoint.
	require.Eventually(t, func() bool { return newHeld.Load() > 0 }, 10*time.Second, 20*time.Millisecond)
}

func TestRebindEndsHeldPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			started <- struct{}{}
			<-r.Context().Done()
			canceled <- struct{}{}
			return
		}
		hub.ServeHTTP(w, r)
	})
	h.startWatcher(t, time.Hour)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("poll did not start")
	}
	key := federationcoord.Key(h.spokeDB.InstanceUID(), h.spokeProject.ID)
	release, err := federationcoord.BeginRebind(t.Context(), key, h.spokeDB, h.spokeProject.ID)
	require.NoError(t, err)
	defer release()
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("held request did not end")
	}
}

func TestHeldRetrySurvivesConcurrentSuccessfulPass(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	h.allowPush(t)
	held := make(chan struct{}, 1)
	var ordinary atomic.Int64
	failHeld := make(chan struct{})
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held <- struct{}{}
			select {
			case <-failHeld:
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusServiceUnavailable)
			case <-r.Context().Done():
			}
			return
		}
		if isEventPoll(r) {
			ordinary.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	retries := NewSyncRetries()
	var armed atomic.Bool
	passFinished := make(chan struct{}, 1)
	passErrors := make(chan error, 1)
	releasePass := make(chan struct{})
	defer func() {
		select {
		case <-releasePass:
		default:
			close(releasePass)
		}
	}()
	runner := &Runner{DB: h.spokeDB, Interval: time.Hour, Retries: retries, OnSpokeResult: func(_ int64, err error) {
		if err != nil && armed.Load() {
			passErrors <- err
			return
		}
		if err == nil && armed.CompareAndSwap(true, false) {
			passFinished <- struct{}{}
			<-releasePass
		}
	}}
	h.startConfiguredRunner(t, runner)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("held poll did not start")
	}
	_, _, err := h.spokeDB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.spokeProject.ID, Title: "Concurrent push", Author: "tester",
	})
	require.NoError(t, err)
	armed.Store(true)
	h.wake <- struct{}{}
	select {
	case <-passFinished:
	case err := <-passErrors:
		t.Fatalf("pass failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("pass did not finish")
	}
	close(failHeld)
	//nolint:kennlint // sleeptest: the held response arrives over real HTTP.
	require.Eventually(t, func() bool { _, ok := retries.FederationSyncRetry(h.spokeProject.ID); return ok }, 10*time.Second, 20*time.Millisecond)
	close(releasePass)
	//nolint:kennlint // sleeptest: allow the completed pass to record its result.
	time.Sleep(200 * time.Millisecond)
	_, ok := retries.FederationSyncRetry(h.spokeProject.ID)
	require.True(t, ok, "a successful pass erased a later Retry-After")
	before := ordinary.Load()
	h.wake <- struct{}{}
	//nolint:kennlint // sleeptest: allow the runner to process the wake.
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, before, ordinary.Load(), "the runner polled before Retry-After")
}

func TestRetiredWatcherDoesNotClearReplacementPoll(t *testing.T) {
	const projectID = 1
	old := &hubLongPoll{cancel: func() {}, done: make(chan struct{})}
	polls := &hubLongPolls{polls: map[int64]*hubLongPoll{projectID: old}}
	polls.setHeld(old, 4, "https://old.example "+strings.Repeat("0", 64)+" 42", func() {})
	polls.stop(projectID)
	replacement := &hubLongPoll{cancel: func() {}, done: make(chan struct{})}
	polls.polls[projectID] = replacement
	polls.setHeld(replacement, 4, "https://new.example "+strings.Repeat("0", 64)+" 42", func() {})
	polls.clearHeld(old)
	require.True(t, hubHoldsPull(withHubLongPolls(t.Context(), polls), projectID, 4, "https://new.example "+strings.Repeat("0", 64)+" 42"), "retired watcher cleared its replacement's held poll")
	polls.setProbed(old, "https://old.example")
	polls.reconcile([]activeSpokeBinding{{binding: db.FederationBinding{ProjectID: projectID, HubURL: "https://new.example"}}})
	require.True(t, hubHoldsPull(withHubLongPolls(t.Context(), polls), projectID, 4, "https://new.example "+strings.Repeat("0", 64)+" 42"), "retired watcher changed its replacement's target")
}

type blockedWatcherRead struct {
	db.Storage
	started chan struct{}
	release chan struct{}
}

func (s *blockedWatcherRead) FederationBindingByProject(ctx context.Context, projectID int64) (db.FederationBinding, error) {
	close(s.started)
	<-s.release
	return s.Storage.FederationBindingByProject(ctx, projectID)
}

func TestStoppedWatcherIsJoinedAtShutdown(t *testing.T) {
	for _, stop := range []struct {
		name string
		stop func(*hubLongPolls, int64)
	}{
		{"failed pass", func(p *hubLongPolls, id int64) { p.stop(id) }},
		{"removed binding", func(p *hubLongPolls, _ int64) { p.reconcile(nil) }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			h := newHubSpokeHarness(t, false)
			synctest.Test(t, func(t *testing.T) {
				store := &blockedWatcherRead{Storage: h.spokeDB, started: make(chan struct{}), release: make(chan struct{})}
				polls := newHubLongPolls(context.WithoutCancel(t.Context()), store, config.DefaultFederationCredentialStore(),
					clientOptsWithDefault(clientpkg.Opts{}), time.Hour, nil, nil, nil, time.Now)
				polls.start(activeSpokeBinding{binding: db.FederationBinding{ProjectID: h.spokeProject.ID}})
				select {
				case <-store.started:
				case <-time.After(10 * time.Second):
					close(store.release)
					polls.stopAll()
					t.Fatal("watcher did not start its read")
				}
				watcher := polls.polls[h.spokeProject.ID]
				stop.stop(polls, h.spokeProject.ID)
				done := make(chan struct{})
				go func() { polls.stopAll(); close(done) }()
				synctest.Wait()
				select {
				case <-done:
					close(store.release)
					<-watcher.done
					t.Fatal("shutdown returned before the retired watcher finished its read")
				default:
				}
				close(store.release)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("shutdown did not join the watcher")
				}
			})
		})
	}
}

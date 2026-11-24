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
	h.holdForever.Store(true)
	h.startRunner(t, 200*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")
	// Let the first held poll prove that the hub holds it.
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(time.Second)
	before := h.pollsWithoutWait.Load()
	lastPull := h.lastPullSuccess(t)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	time.Sleep(2 * time.Second)
	assert.Equal(t, before, h.pollsWithoutWait.Load(), "interval passes polled a hub that was already holding a poll")
	assert.Equal(t, lastPull, h.lastPullSuccess(t), "an interval pass recorded a pull the hub never answered")
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
	proxy.ModifyResponse = func(response *http.Response) error {
		h.hideWatch(response)
		return nil
	}
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
	runner := &Runner{DB: h.spokeDB, Interval: time.Hour}
	h.startConfiguredRunner(t, runner)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the spoke never held a poll at the hub")

	runner.longPolls.mu.Lock()
	done := runner.longPolls.polls[h.spokeProject.ID].done
	runner.longPolls.mu.Unlock()
	failPulls.Store(true)
	h.createHubIssue(t, "Waiting behind a failing pass")
	// The held poll's answer starts a pass, and that pass fails its pull.
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return failed.Load() > 0 },
		10*time.Second, 20*time.Millisecond, "the held poll's answer never started a pass")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not stop after the pass failed")
	}
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
		clientOptsWithDefault(clientpkg.Opts{}), interval, nil, nil, nil, time.Now)
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
}

// A held poll using an old credential must not suppress pulls with a new one.
func TestCredentialRotationEndsTheSkipForTheOldHeldPoll(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	ctx := context.WithoutCancel(t.Context())
	var held, pulls atomic.Int64
	hubURL := h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			held.Add(1)
			<-r.Context().Done()
			return
		}
		if isEventPoll(r) {
			pulls.Add(1)
		}
		hub.ServeHTTP(w, r)
	})
	h.startRunner(t, 200*time.Millisecond)
	//nolint:kennlint // sleeptest: the runner and hub exchange held polls over real HTTP.
	require.Eventually(t, func() bool { return held.Load() > 0 }, 10*time.Second, 20*time.Millisecond)
	before := pulls.Load()
	enrollment, err := h.hubDB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "rotated-pull-token", SpokeInstanceUID: h.spokeDB.InstanceUID(),
		ProjectID: &h.hubProject.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(h.spokeProject.UID, config.FederationCredential{HubURL: hubURL, HubProjectID: h.hubProject.ID, Token: enrollment.Token}))
	h.createHubIssue(t, "Committed after credential rotation")
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP on its interval.
	require.Eventually(t, func() bool { return pulls.Load() > before && h.spokeIssueCount(t) == 1 },
		5*time.Second, 20*time.Millisecond, "interval passes skipped the rotated credential")
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

func TestHeldPollThrottleDelaysIntervalSync(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHubSpokeHarness(t, false)
			answered := make(chan struct{}, 1)
			var held, interval atomic.Int64
			h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
				if isHeldPoll(r) {
					held.Add(1)
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(status)
					select {
					case answered <- struct{}{}:
					default:
					}
					return
				}
				if isEventPoll(r) {
					interval.Add(1)
				}
				hub.ServeHTTP(w, r)
			})
			retries := NewSyncRetries()
			h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: 200 * time.Millisecond, Retries: retries})
			select {
			case <-answered:
			case <-time.After(10 * time.Second):
				t.Fatal("held poll did not reach the hub")
			}
			//nolint:kennlint // sleeptest: the response crosses a real HTTP connection.
			require.Eventually(t, func() bool {
				_, ok := retries.FederationSyncRetry(h.spokeProject.ID)
				return ok
			}, 5*time.Second, 20*time.Millisecond, "the hub's delay was not shared with the runner")
			before := interval.Load()
			h.wake <- struct{}{}
			//nolint:kennlint // sleeptest: allow several runner intervals and a wake.
			time.Sleep(time.Second)
			assert.Equal(t, before, interval.Load(), "the spoke polled before Retry-After")
			assert.Equal(t, int64(1), held.Load(), "the spoke retried the held poll before Retry-After")
			h.rebindTo(t, h.hubURL)
			h.wake <- struct{}{}
			//nolint:kennlint // sleeptest: the runner reads the changed binding after a wake.
			require.Eventually(t, func() bool {
				_, ok := retries.FederationSyncRetry(h.spokeProject.ID)
				return !ok
			}, 5*time.Second, 20*time.Millisecond, "the changed endpoint did not clear the delay")
		})
	}
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
	done := make(chan error, 1)
	go func() {
		release, err := federationcoord.BeginRebind(t.Context(), key, h.spokeDB, h.spokeProject.ID)
		if err == nil {
			release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second): //nolint:kennlint // deadlinetest: rebind must not wait for the whole held poll.
		t.Fatal("an endpoint change waited for the held poll to end")
	}
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("held request did not end")
	}
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
		polls.stop(h.spokeProject.ID)
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
}

func TestHeldPollWakeRunsOnlyItsProject(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	other, hubProject := h.addBinding(t, "other-hub-project", h.url)
	turns := make(chan int64, 16)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: time.Hour, OnSpokeResult: func(id int64, _ error) { turns <- id }})
	//nolint:kennlint // sleeptest: both held polls must reach the hub over HTTP before the commit.
	require.Eventually(t, func() bool { return h.pollsWithWait.Load() >= 2 }, 10*time.Second, 10*time.Millisecond)
	for range 2 {
		select {
		case <-turns:
		case <-time.After(10 * time.Second):
			t.Fatal("initial sync did not finish")
		}
	}
	h.createIssueIn(t, hubProject.ID, "event in one project")
	select {
	case id := <-turns:
		require.Equal(t, other.ID, id, "an unrelated project ran after the wake")
	case <-time.After(10 * time.Second):
		t.Fatal("the changed project was not synced")
	}
	select {
	case id := <-turns:
		t.Fatalf("project %d ran after another project's wake", id)
	case <-time.After(300 * time.Millisecond): //nolint:kennlint // sleeptest: observes extra turns after a real HTTP wake.
	}
}

func TestHeldPollReportsFirstFailure(t *testing.T) {
	h := newHubSpokeHarness(t, false)
	var attempts atomic.Int64
	h.bindThrough(t, func(w http.ResponseWriter, r *http.Request, hub http.Handler) {
		if isHeldPoll(r) {
			attempts.Add(1)
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		hub.ServeHTTP(w, r)
	})
	notices := make(chan error, 8)
	h.startConfiguredRunner(t, &Runner{DB: h.spokeDB, Interval: 100 * time.Millisecond, OnError: func(err error) { notices <- err }})
	select {
	case err := <-notices:
		assert.Contains(t, err.Error(), "held federation poll")
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not report the failed held poll")
	}
	//nolint:kennlint // sleeptest: the watcher retries over HTTP after backing off.
	require.Eventually(t, func() bool { return attempts.Load() >= 2 }, 5*time.Second, 10*time.Millisecond)
	select {
	case err := <-notices:
		t.Fatalf("the watcher repeated its notice: %v", err)
	default:
	}
}

func TestHeldPollWakesKeepEveryProject(t *testing.T) {
	polls := newHubLongPolls(t.Context(), nil, nil, clientpkg.Opts{}, time.Second, nil, nil, nil, time.Now)
	polls.signal(1)
	polls.signal(2)
	<-polls.changed
	require.Equal(t, map[int64]bool{1: true, 2: true, 3: true}, polls.takeChanged(map[int64]bool{3: true}))
}

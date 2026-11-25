package federation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

// countingHub serves a real hub and counts the federation requests it gets.
type countingHub struct {
	store       db.Storage
	server      *httptest.Server
	mu          sync.Mutex
	counts      map[string]int
	log         []string
	refuseBatch int
}

type hubRequestCounts struct{ single, batch, other int }

func newCountingHub(t *testing.T) *countingHub {
	t.Helper()
	return newCountingHubWith(t, nil)
}

// newCountingHubWith lets a test adjust the hub's server configuration, such
// as an embedding host's federation access controller.
func newCountingHubWith(t *testing.T, configure func(*daemon.ServerConfig)) *countingHub {
	t.Helper()
	store, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "hub.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	hub := &countingHub{store: store, counts: map[string]int{}}
	cfg := daemon.ServerConfig{DB: store, FederationBatchPoll: true}
	if configure != nil {
		configure(&cfg)
	}
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "other"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/events:poll":
			kind = "batch"
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Get("issue_uid") == "":
			kind = "single"
		}
		hub.mu.Lock()
		hub.counts[kind]++
		hub.log = append(hub.log, r.Method+" "+r.URL.Path)
		refuse := hub.refuseBatch
		hub.mu.Unlock()
		if kind == "batch" && refuse != 0 {
			// A host in front of the hub that refuses or throttles the route.
			if refuse == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "120")
			}
			http.Error(w, http.StatusText(refuse), refuse)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(hub.server.Close)
	return hub
}

// take returns the counts since the last call.
func (h *countingHub) take() hubRequestCounts {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := hubRequestCounts{single: h.counts["single"], batch: h.counts["batch"], other: h.counts["other"]}
	h.counts = map[string]int{}
	h.log = nil
	return out
}

// requests returns the method and path of each request since the last take,
// in arrival order.
func (h *countingHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.log)
}

type batchPollSpoke struct {
	env       *testenv.Env
	hub       []db.Project
	local     []db.Project
	creds     moveCredentialStore
	errors    map[int64]error
	runner    *Runner
	sharedTok string
	notices   []string
}

// newBatchPollSpoke joins n hub projects with one shared pull and push
// credential. The last hub project is not a federation hub, so the shared
// credential is refused for it.
func newBatchPollSpoke(t *testing.T, hub *countingHub, n int) *batchPollSpoke {
	t.Helper()
	ctx := t.Context()
	s := &batchPollSpoke{env: testenv.New(t), creds: newMoveCredentialStore(), errors: map[int64]error{}, sharedTok: "shared-spoke-token"}
	_, err := hub.store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: s.sharedTok, SpokeInstanceUID: s.env.DB.InstanceUID(), Capabilities: "claim,pull,push", Actor: "worker",
	})
	require.NoError(t, err)
	for i := range n {
		project, err := hub.store.CreateProject(ctx, fmt.Sprintf("hub-project-%d", i))
		require.NoError(t, err)
		if i < n-1 {
			_, err = hub.store.EnableProjectFederation(ctx, project.ID, "worker")
			require.NoError(t, err)
		}
		local, err := s.env.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = s.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: hub.server.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		s.creds[project.UID] = config.FederationCredential{HubURL: hub.server.URL, HubProjectID: project.ID, Token: s.sharedTok, Actor: "worker"}
		s.hub = append(s.hub, project)
		s.local = append(s.local, local)
	}
	s.runner = &Runner{DB: s.env.DB, Credentials: s.creds, OnError: func(err error) {
		if strings.Contains(err.Error(), "batched federation poll") {
			s.notices = append(s.notices, err.Error())
		}
	}, OnSpokeResult: func(projectID int64, err error) {
		if err != nil {
			s.errors[projectID] = err
		} else {
			delete(s.errors, projectID)
		}
	}}
	return s
}

// pass runs one runner pass and returns the refused project's hub refusal.
func (s *batchPollSpoke) pass(t *testing.T) *HubStatusError {
	t.Helper()
	_ = s.runner.RunOnce(t.Context())
	refused := s.local[len(s.local)-1].ID
	for id, err := range s.errors {
		require.Equal(t, refused, id, "only the refused project fails: %v", err)
	}
	status, ok := errors.AsType[*HubStatusError](s.errors[refused])
	require.True(t, ok, "the refused project reports the hub's refusal: %v", s.errors[refused])
	return status
}

func TestRunnerBatchesPollsForProjectsSharingACredential(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 4)

	refusedSingle := spoke.pass(t)
	require.Equal(t, http.StatusForbidden, refusedSingle.StatusCode)
	assert.Equal(t, 4, hub.take().single, "the first pass polls each project and learns the hub serves batches")
	_ = spoke.pass(t)
	hub.take()

	refusedBatch := spoke.pass(t)
	assert.Equal(t, hubRequestCounts{batch: 1}, hub.take(), "an idle pass is one request")
	assert.Equal(t, refusedSingle, refusedBatch, "a refusal inside a batch is reported as the single-project poll reports it")

	issue, _, err := hub.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.hub[1].ID, Title: "hub work", Author: "worker"})
	require.NoError(t, err)
	_ = spoke.pass(t)
	assert.Equal(t, []string{
		"POST /api/v1/federation/events:poll",
		fmt.Sprintf("GET /api/v1/projects/%d/federation/events", spoke.hub[1].ID),
	}, hub.requests(), "only the project with new hub events polls on its own")
	hub.take()
	pulled, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, spoke.local[1].ID, pulled.ProjectID)

	local, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.local[0].ID, Title: "spoke work", Author: "worker"})
	require.NoError(t, err)
	_ = spoke.pass(t)
	assert.Equal(t, hubRequestCounts{batch: 1, single: 1, other: 1}, hub.take(),
		"a project that pushed this turn polls after its push, as before")
	_, err = hub.store.IssueByUID(t.Context(), local.UID, db.IncludeDeletedNo)
	require.NoError(t, err)

	_, err = spoke.env.DB.EnqueuePendingClaim(t.Context(), db.PendingClaimParams{ProjectID: spoke.local[1].ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: spoke.env.DB.InstanceUID(), Holder: "worker", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
	require.NoError(t, err)
	_ = spoke.pass(t)
	project := fmt.Sprintf("/api/v1/projects/%d/", spoke.hub[1].ID)
	var claimed, polledAfterClaim bool
	var polled []string
	for _, request := range hub.requests() {
		switch {
		case strings.HasPrefix(request, "POST "+project+"issues/"):
			claimed = true
		case request == "GET "+project+"federation/events":
			polledAfterClaim = claimed
			polled = append(polled, request)
		case strings.HasSuffix(request, "/federation/events"):
			polled = append(polled, request)
		}
	}
	assert.True(t, claimed, "the queued claim is retried: %v", hub.requests())
	assert.True(t, polledAfterClaim, "the project polls after its claim retry, so the poll can see it: %v", hub.requests())
	assert.Equal(t, []string{"GET " + project + "federation/events"}, polled, "the other projects use the batch")
}

func TestRunnerMovesAnIdleProjectsCursorToTheHubsNewestCheckedEvent(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	before := spoke.spokePasses(t)[0].binding.PullCursorEventID
	_, err := hub.store.CreateProject(t.Context(), "hub-unrelated")
	require.NoError(t, err)
	hub.take()

	_ = spoke.pass(t)
	assert.Equal(t, hubRequestCounts{batch: 1}, hub.take(), "the pass stays one request")
	assert.Greater(t, spoke.spokePasses(t)[0].binding.PullCursorEventID, before,
		"an idle project keeps up with the hub as its own empty poll would")
}

func TestRunnerFallsBackToSingleProjectPollsWhenTheHubRefusesBatches(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{"route not found", http.StatusNotFound},
		{"route forbidden", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub := newCountingHub(t)
			spoke := newBatchPollSpoke(t, hub, 3)
			hub.refuseBatch = test.status
			_ = spoke.pass(t)
			hub.take()

			_ = spoke.pass(t)
			got := hub.take()
			assert.Equal(t, [2]int{1, 3}, [2]int{got.batch, got.single}, "a refused batch falls back to single-project polls")
			_ = spoke.pass(t)
			got = hub.take()
			assert.Equal(t, [2]int{0, 3}, [2]int{got.batch, got.single}, "the refusal holds off further batches")
			require.Len(t, spoke.notices, 1, "the fallback is reported once")
			assert.Contains(t, spoke.notices[0], hub.server.URL)
		})
	}
}

// spokePasses lists the spoke's bindings as a runner pass does.
func (s *batchPollSpoke) spokePasses(t *testing.T) []activeSpokeBinding {
	t.Helper()
	out := make([]activeSpokeBinding, len(s.local))
	for i, project := range s.local {
		binding, err := s.env.DB.FederationBindingByProject(t.Context(), project.ID)
		require.NoError(t, err)
		out[i] = activeSpokeBinding{binding: binding, project: project}
	}
	return out
}

func TestRunnerLeavesARebindingProjectOutOfTheBatch(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 4)
	_ = spoke.pass(t)
	release, err := federationcoord.BeginRebind(t.Context(), federationcoord.Key(spoke.env.DB.InstanceUID(), spoke.local[0].ID), spoke.env.DB, spoke.local[0].ID)
	require.NoError(t, err)
	defer release()

	pages := spoke.runner.prefetchPolls(t.Context(), spoke.spokePasses(t), spoke.creds, func(activeSpokeBinding) bool { return true }, nil)
	assert.NotContains(t, pages, spoke.local[0].ID, "a project being rebound is not polled from its old binding")
	assert.Contains(t, pages, spoke.local[1].ID)
	assert.Contains(t, pages, spoke.local[2].ID)
}

func TestRunnerLeavesAProjectWithAHeldPollOutOfTheBatch(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 4)
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	held := spoke.spokePasses(t)[0].binding
	client, err := NewClient(t.Context(), hub.server.URL, spoke.sharedTok, spoke.runner.clientOpts())
	require.NoError(t, err)
	spoke.runner.longPolls = heldPollFixture(held.ProjectID, held.PullCursorEventID, client.heldPollTarget(held.HubProjectID))

	pages := spoke.runner.prefetchPolls(t.Context(), spoke.spokePasses(t), spoke.creds, func(activeSpokeBinding) bool { return true }, nil)
	assert.NotContains(t, pages, held.ProjectID, "the hub already holds this project's poll at its cursor")
	assert.Contains(t, pages, spoke.local[1].ID)
	assert.Contains(t, pages, spoke.local[2].ID)
}

func TestRunnerDropsAPrefetchedPageReadFromAnotherHubProject(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()
	turn := spoke.spokePasses(t)[0]
	cred := config.FederationTransportCredential(turn.binding.HubURL, turn.binding.HubProjectID, turn.binding.AllowInsecure, spoke.creds[turn.project.UID])
	key, ok := batchPollGroupKey(turn.binding, cred)
	require.True(t, ok)
	stale := &prefetchedFederationPoll{
		result: batchPollResult{Body: api.PollEventsBody{Events: []api.EventEnvelope{}, NextAfterID: turn.binding.PullCursorEventID}},
		key:    key, hubProjectID: turn.binding.HubProjectID + 1000, afterID: turn.binding.PullCursorEventID,
	}

	_, _, err := spoke.runner.runSpoke(context.WithValue(t.Context(), prefetchedFederationPollKey{}, stale), turn, spoke.creds, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, hub.take().single, "a page read for another hub project does not answer this turn")
}

// snapshotCredentialStore counts credential reads.
type snapshotCredentialStore struct {
	moveCredentialStore
	projectReads, snapshots int
}

func (s *snapshotCredentialStore) FederationCredential(ctx context.Context, projectUID string) (config.FederationCredential, bool, error) {
	s.projectReads++
	return s.moveCredentialStore.FederationCredential(ctx, projectUID)
}

func (s *snapshotCredentialStore) FederationCredentialSnapshot(context.Context) (map[string]config.FederationCredential, error) {
	s.snapshots++
	return s.moveCredentialStore, nil
}

func TestRunnerGroupsBatchesFromOneCredentialRead(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 4)
	store := &snapshotCredentialStore{moveCredentialStore: spoke.creds}
	spoke.runner.Credentials = store
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	store.projectReads, store.snapshots = 0, 0

	_ = spoke.pass(t)
	assert.Equal(t, 1, store.snapshots, "grouping reads every credential at once")
	assert.Equal(t, 2*len(spoke.local), store.projectReads, "each project's credential is read under its lock for the batch and again in its turn")
}

func TestBatchPollRefusalHoldsOffATimedOutBatch(t *testing.T) {
	// The client's request timeout surfaces as a url.Error wrapping the
	// deadline, under the generated client's "error sending request" prefix.
	timedOut := fmt.Errorf("error sending request: %w", &url.Error{Op: "Post", URL: "http://daemon.example/api/v1/federation/events:poll", Err: context.DeadlineExceeded})
	_, wholeHub, refused := batchPollRefusal(timedOut, "http://daemon.example")
	assert.True(t, refused)
	assert.True(t, wholeHub, "a timeout pauses every credential on the hub")

	refusedConnection := fmt.Errorf("error sending request: %w", &url.Error{Op: "Post", URL: "http://daemon.example/api/v1/federation/events:poll", Err: syscall.ECONNREFUSED})
	_, _, refused = batchPollRefusal(refusedConnection, "http://daemon.example")
	assert.False(t, refused, "an unreachable hub is reported by each project's own poll")
}

func TestRunnerStopsBatchingAGroupOnceTheHubRefusesItsFirstBatch(t *testing.T) {
	hub := newCountingHub(t)
	// 201 projects fill two full batches of 100 and leave one project over.
	spoke := newBatchPollSpoke(t, hub, 201)
	_ = spoke.pass(t)
	hub.take()
	hub.refuseBatch = http.StatusNotFound

	_ = spoke.pass(t)
	got := hub.take()
	assert.Equal(t, [2]int{1, 201}, [2]int{got.batch, got.single}, "the second 100 projects poll singly instead of sending a batch the hub refuses")
	assert.Len(t, spoke.notices, 1, "the fallback is reported once")
}

// lockPoolStore gives the SQLite spoke the shared project lock a PostgreSQL
// spoke takes: each held lock uses one connection from a fixed pool, and a try
// that finds no free connection fails without waiting.
type lockPoolStore struct {
	db.Storage
	connections chan struct{}
}

func (s *lockPoolStore) TryAcquireFederationProjectSharedLock(context.Context, int64) (func(), bool, error) {
	select {
	case s.connections <- struct{}{}:
		return func() { <-s.connections }, true, nil
	default:
		return nil, false, nil
	}
}

func TestRunnerBatchesAGroupLargerThanTheSpokeLockPool(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 6)
	spoke.runner.DB = &lockPoolStore{Storage: spoke.env.DB, connections: make(chan struct{}, 3)}
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()

	_ = spoke.pass(t)
	assert.Equal(t, hubRequestCounts{batch: 2}, hub.take(), "six projects with three lock connections take two batches and no single polls")
}

func TestRunnerPausesBatchesForTheWholeHubWhenItsRouteIsBlocked(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		batches int
		notices int
		paused  string
	}{
		{"route not found pauses every credential", http.StatusNotFound, 1, 1, "polling its projects"},
		{"forbidden pauses only that credential", http.StatusForbidden, 2, 2, "polling this credential's projects"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub := newCountingHub(t)
			spoke := newBatchPollSpoke(t, hub, 5)
			const secondToken = "second-shared-token"
			_, err := hub.store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
				Token: secondToken, SpokeInstanceUID: spoke.env.DB.InstanceUID(), Capabilities: "claim,pull,push", Actor: "worker",
			})
			require.NoError(t, err)
			for _, project := range spoke.hub[2:4] {
				cred := spoke.creds[project.UID]
				cred.Token = secondToken
				spoke.creds[project.UID] = cred
			}
			_ = spoke.pass(t)
			hub.take()
			hub.refuseBatch = test.status

			_ = spoke.pass(t)
			assert.Equal(t, test.batches, hub.take().batch)
			assert.Len(t, spoke.notices, test.notices)
			for _, notice := range spoke.notices {
				assert.Contains(t, notice, test.paused, "the notice names what the refusal pauses")
			}
		})
	}
}

func TestRunnerHoldsAThrottledGroupInsteadOfPollingItsProjects(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			hub := newCountingHub(t)
			spoke := newBatchPollSpoke(t, hub, 3)
			_ = spoke.pass(t)
			hub.take()
			hub.refuseBatch = status
			// The daemon's runner keeps retry schedules across passes; RunOnce
			// does not.
			retries := NewSyncRetries()

			_, _, _, err := spoke.runner.runOnce(t.Context(), nil, nil, retries)
			require.NoError(t, err)
			assert.Equal(t, hubRequestCounts{batch: 1}, hub.take(), "a throttled batch sends no single polls or pushes")
			_, _, _, err = spoke.runner.runOnce(t.Context(), nil, nil, retries)
			require.NoError(t, err)
			assert.Equal(t, hubRequestCounts{}, hub.take(), "the group's projects wait as a throttled turn would")
			require.Len(t, spoke.notices, 1, "the throttled batch is reported once")
			assert.Contains(t, spoke.notices[0], strconv.Itoa(status))
		})
	}
}

func TestRunnerSendsNoBatchToAHubInCapacityCooldown(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	_ = spoke.pass(t)
	hub.take()
	retries := NewSyncRetries()
	retries.recordHubUnavailable(hub.server.URL, 0, time.Minute, spoke.runner.effectiveInterval(), time.Now())

	_, _, _, err := spoke.runner.runOnce(t.Context(), nil, nil, retries)
	require.NoError(t, err)
	assert.Zero(t, hub.take().batch, "a hub that asked pulls to wait gets no batched poll either")
}

func TestRunnerPutsAHubThatThrottlesABatchIntoCapacityCooldown(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	_ = spoke.pass(t)
	hub.take()
	hub.refuseBatch = http.StatusTooManyRequests
	retries := NewSyncRetries()

	_, _, _, err := spoke.runner.runOnce(t.Context(), nil, nil, retries)
	require.NoError(t, err)
	assert.True(t, retries.hubBlocked(hub.server.URL, time.Now()),
		"a throttled batch pauses the hub's pulls and watches, as a throttled pull does")
}

func TestRunnerKeepsTheHubsWatchAdvertisementAcrossBatchedPasses(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	// A stopped set of watchers still records what hubs advertise, and starts
	// no held polls that would add requests to the counts.
	stopped, stop := context.WithCancel(t.Context())
	stop()
	polls := newHubLongPolls(stopped, nil, nil, clientpkg.Opts{}, time.Second, nil, nil, nil, time.Now)
	spoke.runner.longPolls = polls
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()

	_ = spoke.pass(t)
	require.Equal(t, hubRequestCounts{batch: 1}, hub.take(), "the pass is answered by the batch")
	require.Len(t, polls.hubs, 1)
	for _, support := range polls.hubs {
		assert.True(t, support.takesWatches(time.Now()),
			"a batched page carries no poll headers, so the advertisement from the last single poll stands")
	}
}

// admissionLimitedHub is an embedding host's federation access controller that
// reports its admission full for the polls of one hub project.
type admissionLimitedHub struct {
	mu      sync.Mutex
	limited int64
}

func (a *admissionLimitedHub) AuthorizeFederation(_ context.Context, request daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	a.mu.Lock()
	limited := a.limited
	a.mu.Unlock()
	if request.Operation.ID == "pollFederationProjectEvents" && request.Project.ID == limited {
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostFederationAdmissionLimited
	}
	return daemon.HostFederationAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

func TestRunnerSendsNoFurtherBatchesAfterAProjectIsThrottledInsideOne(t *testing.T) {
	access := &admissionLimitedHub{}
	hub := newCountingHubWith(t, func(cfg *daemon.ServerConfig) { cfg.HostFederationAccess = access })
	spoke := newBatchPollSpoke(t, hub, 5)
	const secondToken = "second-shared-token"
	_, err := hub.store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: secondToken, SpokeInstanceUID: spoke.env.DB.InstanceUID(), Capabilities: "claim,pull,push", Actor: "worker",
	})
	require.NoError(t, err)
	for _, project := range spoke.hub[2:4] {
		cred := spoke.creds[project.UID]
		cred.Token = secondToken
		spoke.creds[project.UID] = cred
	}
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()
	access.mu.Lock()
	access.limited = spoke.hub[0].ID
	access.mu.Unlock()
	retries := NewSyncRetries()

	// The pass reports the throttled project's error.
	_, _, _, _ = spoke.runner.runOnce(t.Context(), nil, nil, retries)
	assert.Equal(t, 1, hub.take().batch,
		"a project the hub throttles inside a batch pauses the hub's other batches, as a throttled pull does")
	assert.True(t, retries.hubBlocked(hub.server.URL, time.Now()))
	retry, waiting := retries.FederationSyncRetry(spoke.local[0].ID)
	require.True(t, waiting, "the throttled project waits as its own throttled pull would")
	assert.Equal(t, http.StatusTooManyRequests, retry.HTTPStatus)
}

func TestRunnerEndsAHubCooldownOnlyAfterAFreshPoll(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 3)
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()
	spokes := spoke.spokePasses(t)
	idle := spokes[0]
	cred := config.FederationTransportCredential(idle.binding.HubURL, idle.binding.HubProjectID, idle.binding.AllowInsecure, spoke.creds[idle.project.UID])
	key, ok := batchPollGroupKey(idle.binding, cred)
	require.True(t, ok)
	page := &prefetchedFederationPoll{
		result:       batchPollResult{Body: api.PollEventsBody{Events: []api.EventEnvelope{}, NextAfterID: idle.binding.PullCursorEventID}},
		key:          key,
		hubProjectID: idle.binding.HubProjectID,
		afterID:      idle.binding.PullCursorEventID,
	}
	// The hub asked pulls to wait after the batch was read, and that wait has
	// since run out, so the turn holds the hub's recovery probe.
	retries := NewSyncRetries()
	retries.recordHubUnavailable(hub.server.URL, 0, 0, spoke.runner.effectiveInterval(), time.Now().Add(-24*time.Hour))
	pull := &hubPullRetryContext{retries: retries, interval: spoke.runner.effectiveInterval(), now: time.Now}
	ctx := context.WithValue(withHubPullRetries(t.Context(), pull), prefetchedFederationPollKey{}, page)

	_, _, err := spoke.runner.runSpoke(ctx, idle, spoke.creds, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, hubRequestCounts{}, hub.take(), "the turn uses the page the batch read")
	_, cooling := retries.hubs[retryHubKey(hub.server.URL)]
	assert.True(t, cooling, "a page read before the hub asked to wait does not show it has recovered")
}

// countingLockPoolStore is a lockPoolStore that reports its pool size, as the
// PostgreSQL store does, and records the most locks held at once.
type countingLockPoolStore struct {
	lockPoolStore
	mu      sync.Mutex
	maxHeld int
}

func (s *countingLockPoolStore) FederationLockCapacity() int { return cap(s.connections) }

func (s *countingLockPoolStore) TryAcquireFederationProjectSharedLock(ctx context.Context, projectID int64) (func(), bool, error) {
	release, ok, err := s.lockPoolStore.TryAcquireFederationProjectSharedLock(ctx, projectID)
	if ok {
		s.mu.Lock()
		s.maxHeld = max(s.maxHeld, len(s.connections))
		s.mu.Unlock()
	}
	return release, ok, err
}

func TestRunnerLeavesSpokeLocksFreeWhileABatchIsInFlight(t *testing.T) {
	hub := newCountingHub(t)
	spoke := newBatchPollSpoke(t, hub, 30)
	store := &countingLockPoolStore{Storage: spoke.env.DB, connections: make(chan struct{}, 25)}
	spoke.runner.DB = store
	_ = spoke.pass(t)
	_ = spoke.pass(t)
	hub.take()

	_ = spoke.pass(t)
	assert.Equal(t, 2, hub.take().batch, "30 projects take a batch of 20 and a batch of 10")
	assert.LessOrEqual(t, store.maxHeld, 25-batchPollLockHeadroom,
		"a batch leaves lock connections for claims and cron forwarding")
}

func TestBatchPollRefusalTreatsAGatewayTimeoutAsATimeout(t *testing.T) {
	summary, wholeHub, refused := batchPollRefusal(&HubStatusError{StatusCode: http.StatusGatewayTimeout}, "http://daemon.example")
	assert.True(t, refused, "a proxy that times out the batch would time it out on every pass")
	assert.True(t, wholeHub)
	assert.Contains(t, summary, "timed out")
}

func TestBatchPollRefusalSaysATimedOutHubDidNotAnswer(t *testing.T) {
	timedOut := fmt.Errorf("error sending request: %w", &url.Error{Op: "Post", URL: "http://daemon.example/api/v1/federation/events:poll", Err: context.DeadlineExceeded})
	summary, _, _ := batchPollRefusal(timedOut, "http://daemon.example")
	assert.Equal(t, "the batched federation poll to hub http://daemon.example timed out", summary)
}

package federation

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

// mapCredentialStore keeps federation credentials in memory.
type mapCredentialStore struct {
	mu          sync.Mutex
	credentials map[string]config.FederationCredential
}

func (s *mapCredentialStore) FederationCredential(_ context.Context, projectUID string) (config.FederationCredential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.credentials[projectUID]
	return credential, ok, nil
}

func (s *mapCredentialStore) StoreFederationCredential(_ context.Context, projectUID string, credential config.FederationCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[projectUID] = credential
	return nil
}

func (s *mapCredentialStore) DeleteFederationCredential(_ context.Context, projectUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.credentials, projectUID)
	return nil
}

// spokeFleet is one spoke daemon bound to many projects on one hub.
type spokeFleet struct {
	store       *sqlitestore.Store
	credentials *mapCredentialStore
	projects    []db.Project
	requests    atomic.Int64
	pushes      atomic.Int64
	// refuse makes the hub answer 403 for a project UID; otherwise it
	// accepts pushes and returns an empty poll.
	refuse sync.Map
	// throttleClaims makes the hub answer 503 to every claim request.
	throttleClaims atomic.Bool
	// beforeAnswer, when set, runs before the hub answers each request.
	beforeAnswer func()
	runner       *Runner
	now          time.Time
}

func newSpokeFleet(tb testing.TB, count int) *spokeFleet {
	tb.Helper()
	ctx := tb.Context()
	tb.Setenv("KATA_HOME", tb.TempDir())
	store, err := testfix.OpenSQLite(ctx, filepath.Join(tb.TempDir(), "spoke.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = store.Close() })
	fleet := &spokeFleet{
		store:       store,
		credentials: &mapCredentialStore{credentials: map[string]config.FederationCredential{}},
		now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	projectsByHubID := map[int64]string{}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fleet.requests.Add(1)
		if r.Method == http.MethodPost {
			fleet.pushes.Add(1)
		}
		var hubProjectID int64
		_, _ = fmt.Sscanf(r.URL.Path, "/api/v1/projects/%d/", &hubProjectID)
		if fleet.beforeAnswer != nil {
			fleet.beforeAnswer()
		}
		if fleet.throttleClaims.Load() && strings.Contains(r.URL.Path, "/lease/") {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if _, refused := fleet.refuse.Load(projectsByHubID[hubProjectID]); refused {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(authInvalidBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var push api.FederationIngestEventsRequestBody
			if err := json.UnmarshalRead(r.Body, &push); err != nil || len(push.Events) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, api.FederationIngestEventsBody{
				Accepted: len(push.Events), PushCursorEventID: push.Events[len(push.Events)-1].EventID,
			})
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"next_after_id":0}`))
	}))
	tb.Cleanup(hub.Close)
	for i := range count {
		project, err := store.CreateProject(ctx, fmt.Sprintf("spoke-project-%02d", i))
		require.NoError(tb, err)
		hubProjectID := int64(100 + i)
		projectsByHubID[hubProjectID] = project.UID
		// Half the bindings push: each holds one local issue the hub has
		// not accepted, so a refused turn is a refused push.
		push := i%2 == 0
		if push {
			_, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "local work", Author: "worker"})
			require.NoError(tb, err)
		}
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke,
			HubURL: hub.URL, HubProjectID: hubProjectID, HubProjectUID: project.UID,
			ReplayHorizonEventID: 1, AllowInsecure: true, Enabled: true, PushEnabled: push, Actor: "worker",
		})
		require.NoError(tb, err)
		fleet.credentials.credentials[project.UID] = config.FederationCredential{
			HubURL: hub.URL, HubProjectID: hubProjectID, Token: "token", AllowInsecure: true,
		}
		fleet.projects = append(fleet.projects, project)
		fleet.refuse.Store(project.UID, true)
	}
	fleet.runner = &Runner{DB: store, Credentials: fleet.credentials, Interval: 30 * time.Second, refusalClock: func() time.Time { return fleet.now }}
	return fleet
}

// pass runs one automatic runner pass at the fleet's current time.
func (f *spokeFleet) pass(ctx context.Context, retries *SyncRetries) {
	_, _, _, _ = f.runner.runOnce(ctx, nil, nil, retries)
}

// simulateHour runs the passes one hour of a runner makes: one per interval
// tick, plus one per wake from a local write every wakeEvery (zero: none).
func (f *spokeFleet) simulateHour(ctx context.Context, retries *SyncRetries, wakeEvery time.Duration) {
	start := f.now
	step := 30 * time.Second
	if wakeEvery > 0 {
		step = min(step, wakeEvery)
	}
	for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += step {
		f.now = start.Add(elapsed)
		if elapsed%(30*time.Second) == 0 || (wakeEvery > 0 && elapsed%wakeEvery == 0) {
			f.pass(ctx, retries)
		}
	}
	f.now = start.Add(time.Hour)
}

// One spoke whose credential the hub refuses for all 50 bound projects. With
// no jitter the refused schedule is 1, 2, 4, 8 and then 15 minutes, so each
// binding is tried at 0, 1, 3, 7, 15, 30 and 45 minutes. Wakes from local
// writes do not bring a refused binding forward.
func TestRefusedSpokeRequestVolumeOverOneHour(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wakeEvery time.Duration
	}{
		{"ticks only", 0},
		{"ticks and a local write every 10s", 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fleet := newSpokeFleet(t, 50)
			fleet.simulateHour(t.Context(), newSyncRetries(noJitter), tc.wakeEvery)
			require.Equal(t, int64(50*7), fleet.requests.Load())
			require.Equal(t, int64(25*7), fleet.pushes.Load(), "the push-enabled half is refused on push")
		})
	}
}

func TestRefusedSpokeRetriesAtOnceAfterCredentialChange(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 2)
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), fleet.requests.Load())
	status, ok := retries.FederationSyncRetry(fleet.projects[0].ID)
	require.True(t, ok)
	require.Equal(t, syncRetryRefused, status.Reason)
	require.Equal(t, http.StatusForbidden, status.HTTPStatus)
	require.Equal(t, fleet.now.Add(time.Minute), status.NextAttemptAt)

	fleet.now = fleet.now.Add(30 * time.Second)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), fleet.requests.Load(), "both bindings wait")

	// The operator rejoins the first project with a new token that the hub
	// accepts. The next pass tries it at once; the other binding keeps
	// waiting.
	credential := fleet.credentials.credentials[fleet.projects[0].UID]
	credential.Token = "replacement"
	require.NoError(t, fleet.credentials.StoreFederationCredential(ctx, fleet.projects[0].UID, credential))
	fleet.refuse.Delete(fleet.projects[0].UID)
	before := fleet.requests.Load()
	fleet.pass(ctx, retries)
	require.Positive(t, fleet.requests.Load()-before)
	_, ok = retries.FederationSyncRetry(fleet.projects[0].ID)
	require.False(t, ok, "the accepted binding has no retry schedule")
	_, ok = retries.FederationSyncRetry(fleet.projects[1].ID)
	require.True(t, ok)
}

func TestRefusedSpokeRetriesAtOnceAfterRebind(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), fleet.requests.Load())

	binding, err := fleet.store.FederationBindingByProject(ctx, fleet.projects[0].ID)
	require.NoError(t, err)
	binding.Enabled = false
	_, err = fleet.store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	fleet.pass(ctx, retries)
	_, ok := retries.FederationSyncRetry(fleet.projects[0].ID)
	require.False(t, ok, "a disabled binding drops its schedule")

	binding.Enabled = true
	_, err = fleet.store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), fleet.requests.Load(), "the re-enabled binding is tried at once")
}

// Disabling and re-enabling a binding between two passes still counts as a
// change: the next pass tries it at once.
func TestRefusedSpokeRetriesAtOnceAfterDisableAndEnableBetweenPasses(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), fleet.requests.Load())

	binding, err := fleet.store.FederationBindingByProject(ctx, fleet.projects[0].ID)
	require.NoError(t, err)
	// The store keeps update times to the millisecond, so toggle until the
	// re-enabled binding's update time has moved.
	for enabled := binding; enabled.UpdatedAt.Equal(binding.UpdatedAt); {
		disabled := binding
		disabled.Enabled = false
		_, err = fleet.store.UpsertFederationBinding(ctx, disabled)
		require.NoError(t, err)
		enabled, err = fleet.store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), fleet.requests.Load(), "the re-enabled binding is tried at once")
}

// A turn's schedule belongs to the binding and credential the turn used. A
// change that lands as the turn ends, here from the result hook while the
// turn still holds the project's sync lock, makes the next pass try at once.
func TestRefusedSpokeRetriesAtOnceAfterChangeAtTurnEnd(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	project := fleet.projects[0]
	changed := false
	fleet.runner.OnSpokeResult = func(int64, error) {
		if changed {
			return
		}
		changed = true
		credential := fleet.credentials.credentials[project.UID]
		credential.Token = "replacement"
		fleet.credentials.credentials[project.UID] = credential
		fleet.refuse.Delete(project.UID)
	}
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), fleet.requests.Load())

	fleet.now = fleet.now.Add(30 * time.Second)
	fleet.pass(ctx, retries)
	require.Greater(t, fleet.requests.Load(), int64(1), "the replacement credential is tried at once")
	_, ok := retries.FederationSyncRetry(project.ID)
	require.False(t, ok)
}

// A token replaced while a request with the old token is in flight is not
// marked refused by that request's answer: the next pass tries it at once.
func TestRefusedSpokeRetriesAtOnceAfterTokenReplacedMidRequest(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	project := fleet.projects[0]
	replaced := false
	fleet.beforeAnswer = func() {
		if replaced {
			return
		}
		replaced = true
		credential := fleet.credentials.credentials[project.UID]
		credential.Token = "replacement"
		require.NoError(t, fleet.credentials.StoreFederationCredential(ctx, project.UID, credential))
	}
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), fleet.requests.Load())
	fleet.refuse.Delete(project.UID)

	fleet.now = fleet.now.Add(30 * time.Second)
	fleet.pass(ctx, retries)
	require.Greater(t, fleet.requests.Load(), int64(1), "the replacement token is tried at once")
	_, ok := retries.FederationSyncRetry(project.ID)
	require.False(t, ok)
}

// When the hub asks a binding to wait, the turn stops: the remaining pending
// claims stay queued and the turn sends no pull or push.
func TestThrottledClaimEndsTheTurn(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	project := fleet.projects[0]
	fleet.refuse.Delete(project.UID)
	fleet.throttleClaims.Store(true)
	for i := range 2 {
		issue, _, err := fleet.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("claimed %d", i), Author: "worker"})
		require.NoError(t, err)
		_, err = fleet.store.EnqueuePendingClaim(ctx, pendingClaimParams(fleet.store, project.ID, issue.ShortID, "worker"))
		require.NoError(t, err)
	}
	retries := newSyncRetries(noJitter)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), fleet.requests.Load(), "only the first claim reaches the hub")
	retry, ok := retries.FederationSyncRetry(project.ID)
	require.True(t, ok)
	require.Equal(t, syncRetryUnavailable, retry.Reason)
	require.Equal(t, fleet.now.Add(2*time.Minute), retry.NextAttemptAt)
	pending, err := fleet.store.ListPendingClaimRequests(ctx, project.ID, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2, "both claims stay queued")
}

// failingClaimAttemptStore cannot record a pending claim attempt.
type failingClaimAttemptStore struct{ *sqlitestore.Store }

func (failingClaimAttemptStore) MarkPendingClaimAttempt(context.Context, string, string, time.Time) error {
	return errors.New("disk full")
}

// A throttled claim still ends the claim loop when the spoke cannot record
// the attempt, and the error keeps both the hub's answer and the local
// failure.
func TestThrottledClaimSurvivesAFailedAttemptWrite(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 1)
	project := fleet.projects[0]
	fleet.refuse.Delete(project.UID)
	fleet.throttleClaims.Store(true)
	for i := range 2 {
		issue, _, err := fleet.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("claimed %d", i), Author: "worker"})
		require.NoError(t, err)
		_, err = fleet.store.EnqueuePendingClaim(ctx, pendingClaimParams(fleet.store, project.ID, issue.ShortID, "worker"))
		require.NoError(t, err)
	}
	binding, err := fleet.store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	err = retryPendingClaimsOnceWithFence(ctx, failingClaimAttemptStore{fleet.store}, binding, fleet.credentials.credentials[project.UID], fleet.runner.clientOpts(), nil)
	require.Equal(t, int64(1), fleet.requests.Load(), "only the first claim reaches the hub")
	require.True(t, hubAskedToWait(err), "the hub's 503 survives: %v", err)
	require.ErrorContains(t, err, "disk full")
	_, _, retryAfter := classifySyncRetry(err)
	require.Equal(t, 2*time.Minute, retryAfter)
}

// failingCredentialStore cannot read any credential.
type failingCredentialStore struct{ *mapCredentialStore }

func (failingCredentialStore) FederationCredential(context.Context, string) (config.FederationCredential, bool, error) {
	return config.FederationCredential{}, false, errors.New("credential store unavailable")
}

// The relay cross-project-links deadline does not depend on the credential,
// so a failed credential read does not let the runner contact the relay early.
func TestRelayRetryHoldsWhenCredentialReadFails(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runner := &Runner{refusalClock: func() time.Time { return now }}
	retries := newSyncRetries(noJitter)
	retries.record(1, [32]byte{}, &HubStatusError{StatusCode: http.StatusConflict, relayCrossProjectLinks: true}, 30*time.Second, now)
	spoke := activeSpokeBinding{binding: db.FederationBinding{ProjectID: 1}}
	require.True(t, runner.retryBlocked(t.Context(), retries, spoke, failingCredentialStore{&mapCredentialStore{}}))
}

// A runner that stops, such as a PostgreSQL leader that loses its lease, stops
// reporting schedules.
func TestRunnerReportsSchedulesOnlyWhileRunning(t *testing.T) {
	fleet := newSpokeFleet(t, 1)
	project := fleet.projects[0]
	retries := newSyncRetries(noJitter)
	fleet.runner.Retries = retries
	// The runner reports a pass's errors after it records the schedules.
	passed := make(chan struct{}, 1)
	fleet.runner.OnError = func(error) {
		select {
		case passed <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- fleet.runner.run(ctx, nil) }()
	<-passed
	_, ok := retries.FederationSyncRetry(project.ID)
	require.True(t, ok, "the refused binding waits")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	_, ok = retries.FederationSyncRetry(project.ID)
	require.False(t, ok, "a stopped runner reports no schedule")
}

// When a hub answers 429 or 503 on one binding, the runner stops contacting
// that hub for the rest of the pass and waits until the hub cooldown expires,
// while bindings on another hub still sync normally.
func TestThrottledHubSkipsRemainingBindingsOnSameHub(t *testing.T) {
	ctx := t.Context()
	fleet := newSpokeFleet(t, 10)
	for _, project := range fleet.projects {
		fleet.refuse.Delete(project.UID)
	}
	// Replace the fleet's hub URL with a throttled hub for the first 9 projects
	// and a healthy second hub for the 10th project.
	var busyHits, healthyHits atomic.Int64
	busyHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		busyHits.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(busyHub.Close)
	healthyHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var push api.FederationIngestEventsRequestBody
			if err := json.UnmarshalRead(r.Body, &push); err != nil || len(push.Events) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.MarshalWrite(w, api.FederationIngestEventsBody{
				Accepted: len(push.Events), PushCursorEventID: push.Events[len(push.Events)-1].EventID,
			})
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"next_after_id":0}`))
	}))
	t.Cleanup(healthyHub.Close)
	for i, project := range fleet.projects {
		targetURL := busyHub.URL
		if i == len(fleet.projects)-1 {
			targetURL = healthyHub.URL
		}
		binding, err := fleet.store.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		binding.HubURL = targetURL
		_, err = fleet.store.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		cred := fleet.credentials.credentials[project.UID]
		cred.HubURL = targetURL
		require.NoError(t, fleet.credentials.StoreFederationCredential(ctx, project.UID, cred))
	}

	retries := newSyncRetries(func(limit time.Duration) time.Duration { return limit / 2 })
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), busyHits.Load(), "a 429 on one binding must stop the remaining bindings on the same hub")
	require.Positive(t, healthyHits.Load(), "a throttled hub must not block bindings on another hub")

	// Inside the Retry-After + jitter window, a second pass sends zero requests to the busy hub.
	fleet.now = fleet.now.Add(65 * time.Second)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(1), busyHits.Load(), "jittered hub cooldown must hold past the base Retry-After")

	// Past Retry-After + jitter (60s + 7.5s = 67.5s), the runner probes the hub again.
	fleet.now = fleet.now.Add(10 * time.Second)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), busyHits.Load(), "the hub should be probed once after its cooldown expires")
}

// BenchmarkRunnerPass times one automatic runner pass over 50 bound projects.
// In "refused" the hub refuses every credential and the pass runs while each
// binding is already waiting on its schedule; "accepted" is the ordinary pass
// with empty polls.
func BenchmarkRunnerPass(b *testing.B) {
	for _, refused := range []bool{true, false} {
		name := "accepted"
		if refused {
			name = "refused"
		}
		b.Run(name, func(b *testing.B) {
			ctx := b.Context()
			fleet := newSpokeFleet(b, 50)
			if !refused {
				for _, project := range fleet.projects {
					fleet.refuse.Delete(project.UID)
				}
			}
			retries := newSyncRetries(noJitter)
			fleet.pass(ctx, retries)
			start := fleet.requests.Load()
			b.ResetTimer()
			for b.Loop() {
				fleet.pass(ctx, retries)
			}
			b.ReportMetric(float64(fleet.requests.Load()-start)/float64(b.N), "requests/op")
		})
	}
}

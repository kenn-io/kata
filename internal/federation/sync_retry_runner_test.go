package federation

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	runner *Runner
	now    time.Time
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
	status, ok := retries.Status(fleet.projects[0].ID)
	require.True(t, ok)
	require.Equal(t, SyncRetryRefused, status.Reason)
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
	_, ok = retries.Status(fleet.projects[0].ID)
	require.False(t, ok, "the accepted binding has no retry schedule")
	_, ok = retries.Status(fleet.projects[1].ID)
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
	_, ok := retries.Status(fleet.projects[0].ID)
	require.False(t, ok, "a disabled binding drops its schedule")

	binding.Enabled = true
	_, err = fleet.store.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	fleet.pass(ctx, retries)
	require.Equal(t, int64(2), fleet.requests.Load(), "the re-enabled binding is tried at once")
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

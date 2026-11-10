package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const relayRefusalBody = `{"error":{"code":"relay_cross_project_links","message":"this daemon's copy of the project has cross-project issue links","hint":"private remedy","data":{"secret":"private diagnostic"}}}`

func TestRelayResetRefusalAttribution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		canonical bool
	}{
		{"refusal", 409, relayRefusalBody, true},
		{"other status", 503, relayRefusalBody, false},
		{"other code", 409, `{"error":{"code":"relay_conflict","message":"other conflict"}}`, false},
		{"malformed", 409, `{"error":{"code":"relay_cross_project_links"}`, false},
		{"truncated", 409, `{"error":{"code":"relay_cross_project_links","message":"` + strings.Repeat("x", 5000) + `"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{baseURL: "https://hub.example", client: &http.Client{Transport: pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: req}, nil
			})}}
			_, err := client.RelayReset(t.Context(), 42)
			var status *HubStatusError
			require.ErrorAs(t, err, &status)
			require.Equal(t, tc.status, status.HTTPStatus())
			require.Equal(t, tc.canonical, errors.Is(err, errRelayCrossProjectLinks))
			if tc.canonical {
				require.Contains(t, err.Error(), "the project on the hub has cross-project issue links")
				require.Contains(t, err.Error(), "remove cross-project related, blocks, and parent links on the hub")
				require.Contains(t, err.Error(), "ask an operator")
				require.NotContains(t, err.Error(), "this daemon's copy")
				require.NotContains(t, err.Error(), "private")
			} else {
				require.Equal(t, tc.body[:min(len(tc.body), 4096)], status.Body)
			}
		})
	}
}

// Override only the binding catalog: sync/status writes still exercise SQLite.
// Refusing metadata uses the same replication transport as a refused reset.
type refusalBindingStore struct {
	db.Storage
	bindings []db.FederationBinding
}

func (s *refusalBindingStore) ListFederationBindings(context.Context) ([]db.FederationBinding, error) {
	return s.bindings, nil
}
func (s *refusalBindingStore) FederationBindingByProject(_ context.Context, id int64) (db.FederationBinding, error) {
	for _, b := range s.bindings {
		if b.ProjectID == id {
			return b, nil
		}
	}
	return db.FederationBinding{}, db.ErrNotFound
}

func TestRunnerRelayRefusalBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, _ := openDaemonclientTestDB(t)
		catalog := &refusalBindingStore{Storage: store}
		credentials := newMoveCredentialStore()
		for i := range 2 {
			project, err := store.CreateProject(t.Context(), fmt.Sprintf("spoke-project-%d", i))
			require.NoError(t, err)
			b, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: int64(i + 1), HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
			require.NoError(t, err)
			if i == 0 {
				b.RelayConfig = &db.RelayBindingConfig{ProtocolVersion: db.RelayProtocolVersion, BindingUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", AuthorityUID: "01HZNQ7VFPK1XGD8R5MABCD4EB", UpstreamInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EB", HubPath: []string{"01HZNQ7VFPK1XGD8R5MABCD4EB", store.InstanceUID()}, LocalActor: "tester", ResetEpoch: 1}
			}
			catalog.bindings = append(catalog.bindings, b)
			credentials[project.UID] = config.FederationCredential{Token: "test-token"}
		}
		var refused, other, results, completed int
		repaired := false
		previous := http.DefaultTransport
		http.DefaultTransport = pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
			status, body := 503, `{"error":{"code":"unavailable"}}`
			if strings.Contains(req.URL.Path, "/projects/1/") {
				refused++
				if !repaired {
					status, body = 409, relayRefusalBody
				}
			} else {
				other++
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		defer func() { http.DefaultTransport = previous }()
		wake := make(chan struct{}, 1)
		runner := &Runner{DB: catalog, Credentials: credentials, Interval: 30 * time.Second, Wake: wake, Debounce: time.Nanosecond, OnSpokeResult: func(id int64, _ error) {
			if id == catalog.bindings[0].ProjectID {
				results++
			}
		}, OnSyncComplete: func() { completed++ }}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runner.run(ctx, nil) }()
		defer func() { cancel(); require.ErrorIs(t, <-done, context.Canceled) }()
		synctest.Wait()
		require.Equal(t, 1, refused)
		last := requireFederationSyncStatus(t, store, catalog.bindings[0].ProjectID)
		wake <- struct{}{}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Equal(t, 1, refused, "wake must not bypass delay")
		require.Equal(t, 1, results, "a skip has no result callback")
		require.Equal(t, 3, completed, "only the other binding completes on wake")
		require.Equal(t, last.LastError, requireFederationSyncStatus(t, store, catalog.bindings[0].ProjectID).LastError)
		// First minute, then two, four, and the five-minute ceiling.
		for i, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
			synctest.Sleep(delay - 30*time.Second)
			synctest.Wait()
			require.Equal(t, i+1, refused, "not due yet")
			synctest.Sleep(30 * time.Second)
			synctest.Wait()
			require.Equal(t, i+2, refused, "due retry")
		}
		require.Greater(t, other, refused, "other projects keep progressing")
		repaired = true
		synctest.Sleep(5 * time.Minute)
		synctest.Wait()
		attempts := refused
		synctest.Sleep(30 * time.Second)
		synctest.Wait()
		require.Equal(t, attempts+1, refused, "a different failure restores ordinary scheduling")
		repaired = false
		synctest.Sleep(30 * time.Second)
		synctest.Wait()
		attempts = refused
		require.Error(t, runner.RunOnce(t.Context()), "synchronous passes stay ungated")
		require.Equal(t, attempts+1, refused)
		// A new reset namespace invalidates the blocked identity immediately.
		catalog.bindings[0].RelayConfig.ResetEpoch++
		wake <- struct{}{}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Equal(t, attempts+2, refused)
		// Observing removal also drops the process-local retry history.
		binding := catalog.bindings[0]
		catalog.bindings = catalog.bindings[1:]
		wake <- struct{}{}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		catalog.bindings = append([]db.FederationBinding{binding}, catalog.bindings...)
		wake <- struct{}{}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Equal(t, attempts+3, refused)
		// A catch-up-only pass is subject to the same gate as ticks and wakes.
		retries := make(relayRefusalRetries)
		_, _, _, err := runner.runPass(t.Context(), nil, nil, retries)
		require.Error(t, err)
		attempts, callbacks := refused, completed
		_, _, remaining, err := runner.runPass(t.Context(), nil, map[int64]bool{binding.ProjectID: true}, retries)
		require.NoError(t, err)
		require.Empty(t, remaining)
		require.Equal(t, attempts, refused)
		require.Equal(t, callbacks, completed)

	})
}

// The checked contract measures delay from completion, honors long configured
// intervals, and forgets a refusal after success or a change of binding identity.
func TestRelayRefusalRetryLifecycle(t *testing.T) {
	for _, interval := range []time.Duration{30 * time.Second, 4 * time.Minute, 10 * time.Minute} {
		t.Run(interval.String(), func(t *testing.T) {
			binding := db.FederationBinding{ProjectID: 1, HubURL: "https://hub.example", HubProjectID: 42, RelayConfig: &db.RelayBindingConfig{BindingUID: "binding", UpstreamInstanceUID: "upstream", ResetEpoch: 1}}
			completed := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
			refused := fmt.Errorf("sync failed: %w", &HubStatusError{StatusCode: 409, relayCrossProjectLinks: true})
			states := make(relayRefusalRetries)
			for i := range 6 {
				states.record(binding, refused, interval, completed)
				delay := max(time.Minute, interval)
				for range i {
					delay = max(interval, min(2*delay, 5*time.Minute))
				}
				require.True(t, states.blocked(binding, completed.Add(delay-time.Nanosecond)))
				require.False(t, states.blocked(binding, completed.Add(delay)))
				completed = completed.Add(delay + 10*time.Second)
			}
			for _, err := range []error{nil, errors.New("different failure")} {
				states.record(binding, err, interval, completed)
				require.False(t, states.blocked(binding, completed))
				states.record(binding, refused, interval, completed)
				require.False(t, states.blocked(binding, completed.Add(max(time.Minute, interval))), "a fresh refusal starts at the first delay")
			}
			for _, change := range []func(*db.FederationBinding){
				func(b *db.FederationBinding) { b.HubURL = "https://other.example" },
				func(b *db.FederationBinding) { b.CreatedAt = completed },
				func(b *db.FederationBinding) { b.RelayConfig.BindingUID = "replacement" },
				func(b *db.FederationBinding) { b.RelayConfig.UpstreamInstanceUID = "replacement" },
				func(b *db.FederationBinding) { b.RelayConfig.ResetEpoch++ },
			} {
				changed := binding
				c := *binding.RelayConfig
				changed.RelayConfig = &c
				change(&changed)
				states.record(binding, refused, interval, completed)
				states.reconcile([]activeSpokeBinding{{binding: changed}})
				require.Empty(t, states)
			}
			states.record(binding, refused, interval, completed)
			states.reconcile(nil) // Includes disabled, archived, and removed bindings.
			require.Empty(t, states)
		})
	}
}

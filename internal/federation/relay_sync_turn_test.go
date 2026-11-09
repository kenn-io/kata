package federation_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// Partial relay turns defer outcome reports, and the runner resumes catch-up
// without waiting for its interval.
func TestRunnerDefersRelayOutcomesUntilCatchUpCompletes(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			relay := newRelayMatrixNode(t, backend, "relay-member")
			project, err := root.store.CreateProject(ctx, "budgeted-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			enrollRelayMatrixReplica(t, root, relay, "budgeted-alias", false)
			syncRelayMatrixNode(t, relay)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))

			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("Backlog issue %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			mirrored := func() int {
				issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
				require.NoError(t, err)
				return len(issues)
			}
			// Recovery waits until the empty fourth batch proves the stream drained;
			// the first three turns leave the outcome observer untouched.
			var results []error
			outcomeRunner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, OnSpokeResult: func(id int64, err error) {
				require.Equal(t, relay.project.ID, id)
				results = append(results, err)
			}}
			for turn, want := range []int{2, 4, 5, 5} {
				require.NoError(t, outcomeRunner.RunOnce(ctx))
				require.Equal(t, want, mirrored(), "turn %d applies one budgeted batch", turn+1)
				if turn < 3 {
					require.Empty(t, results, "partial turns preserve outage state")
				}
			}
			require.Equal(t, []error{nil}, results, "only a full pass reports recovery")

			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("Second backlog issue %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			//nolint:kennlint // sleeptest: The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
			require.Eventually(t, func() bool { return mirrored() == 10 }, 20*time.Second, 20*time.Millisecond, "the runner reschedules budgeted turns instead of waiting an hour")
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
		})
	}
}

// A budgeted turn reschedules only the spokes that stopped with work
// remaining. Another hub this daemon follows is polled once per pass, not once
// per catch-up turn of an unrelated backlog.
func TestRunnerRepollsOnlySpokesWithRemainingWork(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			busy := newRelayMatrixNode(t, backend, "busy-member")
			idle := newRelayMatrixNode(t, backend, "idle-member")
			var idleMetadata atomic.Int32
			idleHandler := idle.http.Config.Handler
			idleProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					idleMetadata.Add(1)
				}
				idleHandler.ServeHTTP(w, r)
			}))
			t.Cleanup(idleProxy.Close)
			idle.http = idleProxy
			relay := newRelayMatrixNode(t, backend, "relay-member")
			for _, root := range []*relayMatrixNode{busy, idle} {
				project, err := root.store.CreateProject(ctx, root.account+"-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				enrollRelayMatrixReplica(t, root, relay, root.account+"-alias", false)
			}
			busyReplica, err := relay.store.ProjectByUID(ctx, busy.project.UID)
			require.NoError(t, err)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))
			for i := range 7 {
				_, _, err := busy.store.CreateIssue(db.WithRootAttribution(ctx, busy.signer, busy.account), db.CreateIssueParams{ProjectID: busy.project.ID, Title: fmt.Sprintf("Busy backlog %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			mirrored := func() int {
				issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: busyReplica.ID})
				require.NoError(t, err)
				return len(issues)
			}
			//nolint:kennlint // sleeptest: The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
			require.Eventually(t, func() bool { return mirrored() == 7 }, 20*time.Second, 20*time.Millisecond)
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			require.EqualValues(t, 1, idleMetadata.Load(), "the idle hub is polled once per pass while the busy spoke catches up")
		})
	}
}

// Restored upstream access is recorded by the first turn; later turns of the
// same on-demand sync must continue from the stored binding rather than retry
// the recovery with the configuration that was already replaced.
func TestOnDemandSyncRecoversRevokedUpstreamAcrossTurns(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			relay := newRelayMatrixNode(t, backend, "relay-member")
			enrollRelayMatrixReplica(t, root, relay, "recovering-alias", true)
			syncRelayMatrixNode(t, relay)
			binding, err := relay.store.FederationBindingByProject(ctx, relay.project.ID)
			require.NoError(t, err)
			revoked := *binding.RelayConfig
			revoked.UpstreamRevoked = true
			binding, err = relay.store.SetRelayBindingConfig(ctx, relay.project.ID, revoked)
			require.NoError(t, err)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))
			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: fmt.Sprintf("Backlog after regrant %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			require.NoError(t, federation.SyncFederationOnce(ctx, relay.store, binding, relay.credential), "turn two must not retry the recovery with the stale revoked config")
			current, err := relay.store.FederationBindingByProject(ctx, relay.project.ID)
			require.NoError(t, err)
			require.False(t, current.RelayConfig.UpstreamRevoked)
			issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 5, "the on-demand sync drains the whole backlog")
		})
	}
}

// Only a successful turn that used its budget runs again immediately. A turn
// that fails waits for the normal interval like a drained project does.
func TestRunnerLeavesFailedTurnsOnTheInterval(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			var metadata atomic.Int32
			rootHandler := root.http.Config.Handler
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") && metadata.Add(1) > 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				rootHandler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			root.http = proxy
			relay := newRelayMatrixNode(t, backend, "relay-member")
			enrollRelayMatrixReplica(t, root, relay, "failing-alias", false)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))
			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: fmt.Sprintf("Backlog before outage %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			failed := make(chan error, 1)
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour, OnError: func(err error) {
				select {
				case failed <- err:
				default:
				}
			}}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			mirrored := func() int {
				issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
				require.NoError(t, err)
				return len(issues)
			}
			select {
			case err := <-failed:
				require.ErrorContains(t, err, "503")
			case <-time.After(20 * time.Second):
				t.Fatal("the second turn did not report the outage")
			}
			//nolint:kennlint // sleeptest: The runner drives real HTTP relay hops; a bounded observation window shows no third attempt follows the failure.
			require.Never(t, func() bool { return metadata.Load() > 2 }, time.Second, 20*time.Millisecond, "the failed second turn is not retried until the interval")
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			require.Equal(t, 2, mirrored(), "the first budgeted turn applied one batch before the outage")
		})
	}
}

// With the daemon's own wiring, pulled events are broadcast to clients and
// hooks but do not wake the runner, so a catch-up on one project still polls
// an unrelated hub once per pass.
func TestRunnerCatchUpIgnoresWakesFromItsOwnPulls(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			busy := newRelayMatrixNode(t, backend, "busy-member")
			idle := newRelayMatrixNode(t, backend, "idle-member")
			var idleMetadata atomic.Int32
			idleHandler := idle.http.Config.Handler
			idleProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					idleMetadata.Add(1)
				}
				idleHandler.ServeHTTP(w, r)
			}))
			t.Cleanup(idleProxy.Close)
			idle.http = idleProxy
			relay := newRelayMatrixNode(t, backend, "relay-member")
			for _, root := range []*relayMatrixNode{busy, idle} {
				project, err := root.store.CreateProject(ctx, root.account+"-project")
				require.NoError(t, err)
				root.project = project
				_, err = root.store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
				require.NoError(t, err)
				public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
				require.NoError(t, root.store.PinRootAuthority(ctx, db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				enrollRelayMatrixReplica(t, root, relay, root.account+"-alias", false)
			}
			busyReplica, err := relay.store.ProjectByUID(ctx, busy.project.UID)
			require.NoError(t, err)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))
			for i := range 7 {
				_, _, err := busy.store.CreateIssue(db.WithRootAttribution(ctx, busy.signer, busy.account), db.CreateIssueParams{ProjectID: busy.project.ID, Title: fmt.Sprintf("Busy backlog %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			broadcaster := daemon.NewEventBroadcaster()
			publisher := daemon.NewEventPublisher(broadcaster, nil)
			var published atomic.Int32
			clients := broadcaster.Subscribe(daemon.SubFilter{})
			t.Cleanup(clients.Unsub)
			go func() {
				for range clients.Ch {
					published.Add(1)
				}
			}()
			wake := make(chan struct{}, 1)
			wakeRunner := func() {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			runCtx, cancel := context.WithCancel(ctx)
			go daemon.ForwardFederationWakes(runCtx, broadcaster, relay.store.InstanceUID(), wakeRunner)
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour, Wake: wake, OnPulledEventsFrom: func(projectID int64, events []db.Event, fork activity.Admission) {
				publisher.EventsFrom(projectID, events, fork)
			}}
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			mirrored := func() int {
				issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: busyReplica.ID})
				require.NoError(t, err)
				return len(issues)
			}
			//nolint:kennlint // sleeptest: The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
			require.Eventually(t, func() bool { return mirrored() == 7 && published.Load() >= 7 }, 20*time.Second, 20*time.Millisecond)
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			require.EqualValues(t, 1, idleMetadata.Load(), "pulled events reach clients but do not restart the pass over idle hubs")
		})
	}
}

// A spoke whose claim retry fails while its relay sync turn uses up the
// budget reports the failure and waits for the interval, even though the turn
// itself left deliveries pending.
func TestRunnerLeavesSpokesWithFailedClaimRetriesOnTheInterval(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRecoveryRoot(t, backend)
			var metadata atomic.Int32
			rootHandler := root.http.Config.Handler
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "acquire") {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/federation/metadata") {
					metadata.Add(1)
				}
				rootHandler.ServeHTTP(w, r)
			}))
			t.Cleanup(proxy.Close)
			root.http = proxy
			relay := newRelayMatrixNode(t, backend, "relay-member")
			enrollRelayMatrixReplica(t, root, relay, "claiming-alias", false)
			syncRelayMatrixNode(t, relay)
			issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
			require.NoError(t, err)
			require.Empty(t, issues)
			seed, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: "Claimed issue", Author: "assistant"})
			require.NoError(t, err)
			syncRelayMatrixNode(t, relay)
			_, err = relay.store.EnqueuePendingClaim(ctx, db.PendingClaimParams{ProjectID: relay.project.ID, IssueRef: seed.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: relay.store.InstanceUID(), Holder: "relay-member", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
			require.NoError(t, err)
			t.Cleanup(federation.SetRelaySyncTurnBudgetForTest(1, 2))
			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: root.project.ID, Title: fmt.Sprintf("Backlog behind a failing claim %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			metadata.Store(0)
			failed := make(chan error, 1)
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour, OnError: func(err error) {
				select {
				case failed <- err:
				default:
				}
			}}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			select {
			case err := <-failed:
				require.ErrorContains(t, err, "503")
			case <-time.After(20 * time.Second):
				t.Fatal("the failed claim retry was not reported")
			}
			//nolint:kennlint // sleeptest: The runner drives real HTTP relay hops; a bounded observation window shows the spoke is not repeated.
			require.Never(t, func() bool { return metadata.Load() > 1 }, time.Second, 20*time.Millisecond, "a turn with a failed claim retry is not repeated before the interval")
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			issues, err = relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 3, "the sync turn ran and stopped at its budget with work remaining")
		})
	}
}

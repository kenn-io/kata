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

// A root checkpoint can withhold a cross-project creation while retaining a
// later assignment and snapshot. The relay must forward the signed checkpoint
// rather than replay the assignment before the leaf knows the issue.
func TestRelayLeafRecoversLateSnapshot(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			root := newRelayMatrixNode(t, backend, "root-member")
			relay := newRelayMatrixNode(t, backend, "relay-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(ctx, "shared-project")
			require.NoError(t, err)
			root.project = project
			known, knownCreation, err := root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Accepted prefix issue"})
			require.NoError(t, err)
			privateProject, err := root.store.CreateProject(ctx, "unshared-project")
			require.NoError(t, err)
			privateIssue, _, err := root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: privateProject.ID, Author: "source-agent", Title: "Private peer"})
			require.NoError(t, err)
			issue, creation, err := root.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Author: "source-agent", Title: "Shared issue", Links: []db.InitialLink{{ToNumber: privateIssue.ID, Type: "related"}}})
			require.NoError(t, err)
			links, err := root.store.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.NoError(t, root.store.DeleteLinkByID(ctx, links[0].ID))
			owner := "source-agent"
			assigned, err := root.store.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: owner})
			require.NoError(t, err)
			assignment := assigned.Event
			require.Equal(t, "issue.assigned", assignment.Type)
			_, err = root.store.EnableProjectFederation(ctx, project.ID, root.account)
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			pin := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
			require.NoError(t, root.store.PinRootAuthority(ctx, pin))
			enrollRelayMatrixReplica(t, root, relay, "relay-alias", true)
			syncRelayMatrixNode(t, relay)
			history, err := relay.store.EventsByUIDs(ctx, relay.project.ID, []string{assignment.UID})
			require.NoError(t, err)
			require.Len(t, history, 1, "the private creation was withheld while the assignment survived")
			require.Equal(t, assignment.UID, history[0].UID)
			require.Equal(t, assignment.ContentHash, history[0].ContentHash)
			_, err = relay.store.EventsByUIDs(ctx, relay.project.ID, []string{creation.UID})
			require.ErrorIs(t, err, db.ErrNotFound, "private historical creation remains withheld")
			enrollRelayMatrixReplica(t, relay, leaf, "leaf-alias", false)
			binding, err := leaf.store.FederationBindingByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			required, err := relay.store.(db.RelayResetBootstrapStore).RelayEnrollmentNeedsReset(ctx, binding.RelayConfig.BindingUID)
			require.NoError(t, err)
			require.True(t, required, "a retained late snapshot must select signed reset")

			// Reproduce the pre-repair failed leaf replay with the same live
			// enrollment. Unknown-issue validation stays strict and atomic.
			body, err := db.EncodeRelaySourceEvent(db.RemoteEventFromStored(knownCreation))
			require.NoError(t, err)
			envelope, err := db.SealRelayEnvelope(db.RelayEnvelope{Version: db.RelayProtocolVersion, BindingUID: binding.RelayConfig.BindingUID, ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), SenderInstanceUID: relay.store.InstanceUID(), ReceiverInstanceUID: leaf.store.InstanceUID(), Epoch: binding.RelayConfig.ResetEpoch, Sequence: 1, Stream: db.RelayStreamEvent, Path: []string{root.store.InstanceUID(), relay.store.InstanceUID()}, SourceUID: knownCreation.UID, SourceHash: knownCreation.ContentHash, Body: body})
			require.NoError(t, err)
			_, err = leaf.store.AcceptRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
			require.NoError(t, err, "an older replay accepted this prefix before failing on the missing issue")
			body, err = db.EncodeRelaySourceEvent(db.RemoteEventFromStored(history[0]))
			require.NoError(t, err)
			envelope.Sequence = 2
			envelope.SourceUID, envelope.SourceHash, envelope.Body = assignment.UID, assignment.ContentHash, body
			envelope, err = db.SealRelayEnvelope(envelope)
			require.NoError(t, err)
			_, err = leaf.store.AcceptRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayBatch{Stream: db.RelayStreamEvent, Envelopes: []db.RelayEnvelope{envelope}})
			require.ErrorIs(t, err, db.ErrFederationIngestValidation)
			require.ErrorContains(t, err, "unknown issue")
			_, err = leaf.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.ErrorIs(t, err, db.ErrNotFound)

			checkpoint, err := relay.store.(db.RelayResetStore).CreateRelayReset(ctx, binding.RelayConfig.BindingUID, db.RootAttributionSigner{})
			require.NoError(t, err, "a relay forwards the existing root signature without a root private key")
			require.NoError(t, db.VerifyRootResetManifest(pin, checkpoint.Manifest, checkpoint.Snapshot))
			install := func() error {
				return leaf.store.(db.RelayResetStore).InstallRelayReset(ctx, binding.RelayConfig.BindingUID, checkpoint.Manifest, checkpoint.Snapshot, checkpoint.Translation)
			}
			claim, err := leaf.store.EnqueuePendingClaim(ctx, db.PendingClaimParams{ProjectID: leaf.project.ID, IssueRef: known.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: leaf.store.InstanceUID(), Holder: "leaf-member", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
			require.NoError(t, err)
			require.ErrorIs(t, install(), db.ErrFederationResetBlockedByPendingPush)
			claims, err := leaf.store.ListPendingClaimRequests(ctx, leaf.project.ID, 100)
			require.NoError(t, err)
			require.Len(t, claims, 1, "blocked reset retains the pending claim")
			require.Equal(t, claim.RequestUID, claims[0].RequestUID)
			require.NoError(t, leaf.store.RejectPendingClaim(ctx, claim.RequestUID, "Fixture resolves its pending claim", time.Now().UTC()))
			quarantine, err := leaf.store.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{ProjectID: leaf.project.ID, Direction: db.FederationQuarantineDirectionPull, FirstEventID: 2, LastEventID: 2, EventUIDs: []string{assignment.UID}, Error: "Previous replay rejected an unknown issue", RelayBindingUID: binding.RelayConfig.BindingUID, RelayResetEpoch: binding.RelayConfig.ResetEpoch})
			require.NoError(t, err)
			require.ErrorIs(t, install(), db.ErrFederationResetBlockedByQuarantine)
			active, err := leaf.store.ActiveFederationQuarantinesByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			require.Len(t, active, 1, "blocked reset retains quarantine for explicit recovery")
			_, err = leaf.store.RetryFederationQuarantine(ctx, db.RetryFederationQuarantineParams{ID: quarantine.ID, ProjectID: leaf.project.ID, Actor: leaf.account, Reason: "Retry through signed checkpoint", Now: time.Now().UTC()})
			require.NoError(t, err)
			local, localEvent, err := leaf.store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: leaf.project.ID, Author: "leaf-agent", Title: "Pending local issue"})
			require.NoError(t, err)
			require.ErrorIs(t, install(), db.ErrFederationResetBlockedByPendingPush)
			pending, err := leaf.store.PendingRelayDeliveries(ctx, binding.RelayConfig.BindingUID, db.RelayStreamEvent, 100)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			require.Equal(t, localEvent.UID, pending[0].SourceUID)
			err = federation.SyncFederationOnce(ctx, leaf.store, binding, leaf.credential)
			require.ErrorIs(t, err, db.ErrFederationResetBlockedByPendingPush, "root acceptance is required even after the relay acknowledges local work")
			syncRelayMatrixNode(t, relay)
			syncRelayMatrixNode(t, leaf)
			got, err := leaf.store.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, issue.Title, got.Title)
			require.NotNil(t, got.Owner)
			require.Equal(t, owner, *got.Owner)
			quarantines, err := leaf.store.ActiveFederationQuarantinesByProject(ctx, leaf.project.ID)
			require.NoError(t, err)
			require.Empty(t, quarantines)
			retained, err := leaf.store.IssueByUID(ctx, local.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, local.Title, retained.Title, "the recovered checkpoint retains accepted local work")
		})
	}
}

// One relay sync turn moves a bounded number of batches per stream and then
// yields with work remaining; the next turn continues where it stopped, and
// the runner comes back without waiting for its interval.
func TestRelaySyncTurnYieldsWithWorkRemaining(t *testing.T) {
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
			binding, err := relay.store.FederationBindingByProject(ctx, relay.project.ID)
			require.NoError(t, err)
			mirrored := func() int {
				issues, err := relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
				require.NoError(t, err)
				return len(issues)
			}
			// The third turn's only batch is short, but the stream and byte limits
			// also cut batches short, so a turn that used its budget on a nonempty
			// batch still reports work; only an empty batch proves the stream drained.
			for turn, want := range []int{2, 4, 5, 5} {
				remaining, err := federation.SyncFederationTurnForTest(ctx, relay.store, binding, relay.credential)
				require.NoError(t, err)
				require.Equal(t, want, mirrored(), "turn %d applies one budgeted batch", turn+1)
				require.Equal(t, turn < 3, remaining, "turn %d reports whether deliveries may remain", turn+1)
			}

			for i := range 5 {
				_, _, err := root.store.CreateIssue(db.WithRootAttribution(ctx, root.signer, root.account), db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("Second backlog issue %d", i), Author: "assistant"})
				require.NoError(t, err)
			}
			runner := &federation.Runner{DB: relay.store, Credentials: relay.credentials, Interval: time.Hour}
			runCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- runner.Run(runCtx) }()
			//nolint:kennlint // The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
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
			//nolint:kennlint // The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
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
			//nolint:kennlint // The runner drives real HTTP relay hops; a bounded observation window shows no third attempt follows the failure.
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
			//nolint:kennlint // The runner drives real HTTP relay hops; bounded polling observes the mirrored projection.
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
			//nolint:kennlint // The runner drives real HTTP relay hops; a bounded observation window shows the spoke is not repeated.
			require.Never(t, func() bool { return metadata.Load() > 1 }, time.Second, 20*time.Millisecond, "a turn with a failed claim retry is not repeated before the interval")
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			issues, err = relay.store.ListIssues(ctx, db.ListIssuesParams{ProjectID: relay.project.ID})
			require.NoError(t, err)
			require.Len(t, issues, 3, "the sync turn ran and stopped at its budget with work remaining")
		})
	}
}

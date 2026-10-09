package federation_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	clientpkg "go.kenn.io/kata/internal/client"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R3/R4/R9: a persisted candidate enrollment is a retry identity, not live
// transport authority. No legacy or relay fetch starts before activation.
func TestRelayEnrollmentPendingSuppressesTransport(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			node := newRelayMatrixNode(t, backend, "local-member")
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(remote.Close)
			project, err := node.store.CreateProject(t.Context(), "pending-replica")
			require.NoError(t, err)
			binding, err := node.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: remote.URL, HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Actor: "local-member", Enabled: true})
			require.NoError(t, err)
			candidate := config.FederationCredential{HubURL: remote.URL, HubProjectID: 42, Token: "candidate-enrollment-test-token", Actor: "local-member", Capabilities: "claim,pull,push", AllowInsecure: true, RelayEnrollmentPending: true}
			err = federation.SyncFederationOnce(t.Context(), node.store, binding, candidate)
			require.Error(t, err)
			require.Zero(t, calls.Load(), "pending enrollment must not reach any transport endpoint")
			require.ErrorContains(t, err, "enrollment pending")
			retained, err := node.store.FederationBindingByProject(t.Context(), project.ID)
			require.NoError(t, err)
			require.Zero(t, retained.PullCursorEventID)
			require.Zero(t, retained.PushCursorEventID)
		})
	}
}

// R4: offline claims keep their exact durable identity until the candidate
// enrollment is active; a pending token must not be dispatched by the runner.
func TestRelayEnrollmentPendingSuppressesClaimRetry(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			node := newRelayMatrixNode(t, backend, "local-member")
			var calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(remote.Close)
			project, err := node.store.CreateProject(t.Context(), "pending-claim-retry")
			require.NoError(t, err)
			issue, _, err := node.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Pending offline claim", Author: "local-member"})
			require.NoError(t, err)
			binding, err := node.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: remote.URL, HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Actor: "local-member", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			pending, err := node.store.EnqueuePendingClaim(t.Context(), db.PendingClaimParams{ProjectID: project.ID, IssueRef: issue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: node.store.InstanceUID(), Holder: "local-member", ClientKind: "cli"}, ClaimKind: "hard", Now: time.Now().UTC()})
			require.NoError(t, err)
			candidate := config.FederationCredential{HubURL: remote.URL, HubProjectID: 42, Token: "candidate-claim-test-token", Actor: "local-member", Capabilities: "claim,pull,push", AllowInsecure: true, RelayEnrollmentPending: true}
			err = federation.RetryPendingClaimsOnce(t.Context(), node.store, binding, candidate, clientpkg.Opts{})
			require.Error(t, err)
			require.Zero(t, calls.Load(), "pending enrollment must not dispatch offline claims")
			require.ErrorContains(t, err, "enrollment pending")
			retained, err := node.store.ListPendingClaimRequests(t.Context(), project.ID, 10)
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Equal(t, pending.RequestUID, retained[0].RequestUID)
		})
	}
}

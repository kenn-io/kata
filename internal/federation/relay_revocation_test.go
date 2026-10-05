package federation_test

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

// R6/A4: after learning that its upstream grant is revoked, a personal
// relay must stop admitting downstream traffic. Temporary unavailability
// is not revocation and does not discard queued source events.
func TestRelayUpstreamRevocationStopsDescendants(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			root := newRelayMatrixNode(t, backend, "company-member")
			personal := newRelayMatrixNode(t, backend, "personal-member")
			leaf := newRelayMatrixNode(t, backend, "leaf-member")
			project, err := root.store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			root.project = project
			_, err = root.store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleHub, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true})
			require.NoError(t, err)
			public := root.signer.PrivateKey.Public().(ed25519.PublicKey)
			require.NoError(t, root.store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: root.store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
			enrollRelayMatrixReplica(t, root, personal, "personal-alias", true)
			enrollRelayMatrixReplica(t, personal, leaf, "leaf-alias", false)
			unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			}))
			t.Cleanup(unavailable.Close)
			outageCredential := personal.credential
			outageCredential.HubURL = unavailable.URL
			binding, err := personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.Error(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, outageCredential))
			binding, err = personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.False(t, binding.RelayConfig.UpstreamRevoked, "temporary outage is not revocation")
			syncRelayMatrixNode(t, personal)
			grants, err := root.store.ListFederationEnrollments(t.Context())
			require.NoError(t, err)
			require.Len(t, grants, 1)
			require.NoError(t, root.store.RevokeFederationEnrollment(t.Context(), grants[0].ID))
			require.Error(t, federation.SyncFederationOnce(t.Context(), personal.store, binding, personal.credential))
			binding, err = personal.store.FederationBindingByProject(t.Context(), personal.project.ID)
			require.NoError(t, err)
			require.True(t, binding.RelayConfig.UpstreamRevoked, "known upstream rejection must be durable")
			_, err = personal.store.AuthorizeFederationToken(t.Context(), leaf.credential.Token, personal.project.ID, "pull")
			require.Error(t, err, "known upstream revocation closes downstream admission")
		})
	}
}

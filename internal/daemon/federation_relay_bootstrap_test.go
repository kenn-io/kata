package daemon_test

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R3/R9: an enabled root must enroll its first relay with the daemon's existing
// signing key, without waiting for another issue mutation to establish a pin.
// A restored public pin cannot silently switch to a different private key.
func TestRelayEnrollmentInitialRootSigningPin(t *testing.T) {
	for _, mode := range []string{"fresh", "key_conflict"} {
		t.Run(mode, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				fixture := newProjectAccessFixture(t, store)
				_, err := store.EnableProjectFederation(t.Context(), fixture.private.ID, "admin")
				require.NoError(t, err)
				public, private, err := ed25519.GenerateKey(nil)
				require.NoError(t, err)
				var previous db.RootKeyPin
				if mode == "key_conflict" {
					previousPublic, _, err := ed25519.GenerateKey(nil)
					require.NoError(t, err)
					previous = db.RootKeyPin{ProjectUID: fixture.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(previousPublic), PublicKey: previousPublic}
					require.NoError(t, store.PinRootAuthority(t.Context(), previous))
				}
				signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: private}
				server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, RootAttributionSigner: &signer})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				endpoint := httptest.NewServer(server.Handler())
				t.Cleanup(endpoint.Close)
				request := projectAccessFixture{store: store, server: endpoint}
				body := map[string]any{"project_id": fixture.private.ID, "spoke_instance_uid": "00000000000000000000000002", "token": "narrow-enrollment-test-token", "capabilities": "claim,pull,push", "relay": map[string]any{"protocol_version": 1, "serve_downstream": true}}
				code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
				if mode == "key_conflict" {
					require.Equal(t, http.StatusConflict, code, string(raw))
					pin, err := store.RootAuthority(t.Context(), fixture.private.UID)
					require.NoError(t, err)
					require.Equal(t, previous.KeyID, pin.KeyID)
					enrollments, err := store.ListFederationEnrollments(t.Context())
					require.NoError(t, err)
					require.Empty(t, enrollments)
					return
				}
				require.Equal(t, http.StatusOK, code, string(raw))
				pin, err := store.RootAuthority(t.Context(), fixture.private.UID)
				require.NoError(t, err)
				require.Equal(t, db.RootPublicKeyID(public), pin.KeyID)
				require.Equal(t, store.InstanceUID(), pin.AuthorityUID)
				require.Contains(t, string(raw), pin.KeyID)
				// An exact replay preserves the same active root pin and narrow grant.
				code, _, raw = request.request(t, http.MethodPost, "/api/v1/federation/enrollments", "member", body, nil)
				require.Equal(t, http.StatusOK, code, string(raw))
				enrollments, err := store.ListFederationEnrollments(t.Context())
				require.NoError(t, err)
				require.Len(t, enrollments, 1)
			})
		})
	}
}

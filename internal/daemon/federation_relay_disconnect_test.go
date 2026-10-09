package daemon_test

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R9: possession of a narrow relay credential may only attenuate that exact
// grant. Cleanup remains idempotent after parent revocation and exposes no data.
func TestRelayDisconnectOwnGrant(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.EnableProjectFederation(t.Context(), f.private.ID, "admin")
		require.NoError(t, err)
		public, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		require.NoError(t, store.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
		parent, err := store.ResolveAPIToken(t.Context(), "member-test-token")
		require.NoError(t, err)
		peer := "00000000000000000000000008"
		grant, err := store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: peer, ProtocolVersion: db.RelayProtocolVersion, Token: "disconnect-narrow-test-token"})
		require.NoError(t, err)
		sibling, err := store.CreateRelayEnrollment(t.Context(), db.CreateRelayEnrollmentParams{ProjectID: f.private.ID, ParentTokenID: parent.ID, SpokeInstanceUID: "00000000000000000000000009", ProtocolVersion: db.RelayProtocolVersion, Token: "disconnect-sibling-test-token"})
		require.NoError(t, err)
		legacy, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ProjectID: &f.private.ID, SpokeInstanceUID: peer, Actor: "member", Capabilities: "pull,push", Token: "disconnect-legacy-test-token"})
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", f.private.ID)
		body := map[string]any{"spoke_instance_uid": peer}
		code, _, raw := f.request(t, http.MethodPost, path, "", body, map[string]string{"Authorization": "Bearer " + grant.Token})
		require.Equal(t, http.StatusOK, code, string(raw))
		for _, token := range []string{"invalid-test-token", "member-test-token", "bootstrap-test-token", legacy.Token} {
			code, _, raw := f.request(t, http.MethodPost, path, "", body, map[string]string{"Authorization": "Bearer " + token})
			require.Equal(t, http.StatusNotFound, code, string(raw))
		}
		code, _, raw = f.request(t, http.MethodPost, path, "", map[string]any{"spoke_instance_uid": "00000000000000000000000009"}, map[string]string{"Authorization": "Bearer " + grant.Token})
		require.Equal(t, http.StatusNotFound, code, string(raw))
		code, _, raw = f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/federation/relay:disconnect", f.public.ID), "", body, map[string]string{"Authorization": "Bearer " + grant.Token})
		require.Equal(t, http.StatusNotFound, code, string(raw))
		_, _, err = store.RevokeAPIToken(t.Context(), parent.ID, "admin")
		require.NoError(t, err)
		for range 2 {
			code, _, raw = f.request(t, http.MethodPost, path, "", body, map[string]string{"Authorization": "Bearer " + grant.Token})
			require.Equal(t, http.StatusOK, code, string(raw))
			require.JSONEq(t, `{"revoked":true}`, string(raw))
			require.NotContains(t, string(raw), grant.Token)
		}
		enrollments, err := store.ListFederationEnrollments(t.Context())
		require.NoError(t, err)
		require.Len(t, enrollments, 3)
		for _, retained := range enrollments {
			switch retained.ID {
			case grant.Enrollment.ID:
				require.NotNil(t, retained.RevokedAt)
			case sibling.Enrollment.ID, legacy.Enrollment.ID:
				require.Nil(t, retained.RevokedAt)
			default:
				t.Fatalf("unexpected enrollment %d", retained.ID)
			}
		}
	})
}

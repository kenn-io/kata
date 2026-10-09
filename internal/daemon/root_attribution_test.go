package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"net/http/httptest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R5: root-native receipts use authenticated credentials; owner-local writes
// retain the established request-actor convention without granting a token
// principal owner authority. Source/event and proof commit together.
func TestRootAttributionRequestAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}
		cfg := daemon.ServerConfig{DB: store, RootAttributionSigner: &signer, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}}
		server := daemon.NewServer(cfg)
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		httpServer := httptest.NewServer(server.Handler())
		t.Cleanup(httpServer.Close)
		f.server = httpServer
		root := fmt.Sprintf("/api/v1/projects/%d/issues", f.private.ID)
		status, _, raw := f.request(t, http.MethodPost, root, "member", map[string]any{"actor": "impostor", "title": "Authenticated root task", "force_new": true}, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var result struct {
			Issue db.Issue `json:"issue"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.NotEmpty(t, result.Issue.UID, string(raw))
		proof, err := store.EntityAttribution(t.Context(), f.private.UID, "issue", result.Issue.UID)
		require.NoError(t, err)
		require.Equal(t, "member", proof.AccountableActor)
		require.Equal(t, "member", proof.SourceActor)
		require.Equal(t, store.InstanceUID(), proof.IngressInstanceUID)
		require.NoError(t, db.VerifyRootReceipt(db.RootKeyPin{ProjectUID: f.private.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(pub), PublicKey: pub}, proof))
		status, _, raw = f.request(t, http.MethodPost, root, "nonmember", map[string]any{"actor": "member", "title": "Forbidden task"}, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		_, err = store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
		require.NoError(t, err)
		status, _, raw = f.request(t, http.MethodPost, root, "member", map[string]any{"actor": "member", "title": "Revoked task"}, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))

		// A true socket/local owner may write a restricted project without borrowing
		// a team membership. This is the existing local actor contract.
		ownerServer := daemon.NewServer(daemon.ServerConfig{DB: store, RootAttributionSigner: &signer})
		t.Cleanup(func() { require.NoError(t, ownerServer.Close()) })
		ownerHTTP := httptest.NewServer(ownerServer.Handler())
		t.Cleanup(ownerHTTP.Close)
		f.server = ownerHTTP
		status, _, raw = f.request(t, http.MethodPost, root, "", map[string]any{"actor": "local-owner", "title": "Local owner task", "force_new": true}, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &result))
		proof, err = store.EntityAttribution(t.Context(), f.private.UID, "issue", result.Issue.UID)
		require.NoError(t, err)
		require.Equal(t, "local-owner", proof.AccountableActor)
		require.Equal(t, "local-owner", proof.SourceActor)
	})
}

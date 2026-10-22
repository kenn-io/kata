package daemon_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// On a relay, only the account that connected the bridge can enroll a
// downstream daemon that will itself serve the project onward. Any member may
// still enroll a leaf.
func TestRelayDownstreamEnrollmentRequiresBridgeConnector(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	for _, actor := range []string{"local-member", "other-member"} {
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{Actor: actor, AdminActor: "admin", PlaintextToken: actor + "-test-token"})
		require.NoError(t, err)
	}
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token", RequireTokenIdentity: true}, FederationCredentials: config.DefaultFederationCredentialStore(), FederationCatalog: hub.catalog(hub.root.server.URL)})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	local := projectAccessFixture{store: store, server: endpoint}
	body := bridgeConnectBody(hub.root.private.Name, false)
	body["serve_downstream"] = true
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", body, bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)

	enroll := func(actor, token, peer string, serveDownstream bool) (int, []byte) {
		t.Helper()
		request := map[string]any{"spoke_instance_uid": peer, "project_id": project.ID, "capabilities": "claim,pull,push", "token": token, "relay": map[string]any{"protocol_version": 1, "serve_downstream": serveDownstream}}
		code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/enrollments", actor, request, nil)
		return code, raw
	}
	//nolint:gosec // These are deterministic fixture credentials, never operational secrets.
	code, raw = enroll("other-member", "other-downstream-test-token", "00000000000000000000000006", true)
	require.Equal(t, http.StatusForbidden, code, string(raw))
	require.Equal(t, "relay_downstream_forbidden", bridgeErrorCode(t, raw))
	_, err = store.AuthorizeFederationToken(t.Context(), "other-downstream-test-token", project.ID, "pull")
	require.ErrorIs(t, err, db.ErrNotFound, "a refused downstream enrollment issues no grant")

	code, raw = enroll("other-member", "other-leaf-test-token", "00000000000000000000000006", false)
	require.Equal(t, http.StatusOK, code, string(raw))
	code, raw = enroll("local-member", "connector-downstream-test-token", "00000000000000000000000007", true)
	require.Equal(t, http.StatusOK, code, string(raw))
	grants, err := store.ListFederationEnrollments(t.Context())
	require.NoError(t, err)
	require.Len(t, grants, 2)
	for _, grant := range grants {
		require.Equal(t, grant.Actor == "local-member", grant.RelayServeDownstream, "only the connector's grant serves downstream")
	}
}

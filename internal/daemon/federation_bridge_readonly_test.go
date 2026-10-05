package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R9: a compatible read-only peer must be reported before project resolution
// or enrollment, for preview as well as connect. No saved credential is issued.
func TestFederationBridgeRejectsReadonlyPeer(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		var calls atomic.Int32
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/api/v1/instance" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"instance_uid":"00000000000000000000000002","relay_protocol_version":1,"provenance_protocol_version":1,"embedding_artifact_protocol_version":1,"web_ui_capabilities":{"writable":false},"auth":{"kind":"db_token","actor":"member"}}`)
		}))
		t.Cleanup(peer.Close)
		credentials := newReplicaCredentialStore()
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials, FederationCatalog: []config.CatalogDaemonConfig{{Name: "example-hub", URL: peer.URL, Token: "ordinary-user-test-token", AllowInsecure: true}}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		request := projectAccessFixture{store: store, server: endpoint}
		for _, preflight := range []bool{true, false} {
			before := calls.Load()
			code, _, raw := request.request(t, http.MethodPost, "/api/v1/federation/bridges", "", map[string]any{"hub_catalog": "example-hub", "hub_project": "shared-project", "project_name": "shared-replica", "actor": "local-member", "preflight": preflight}, map[string]string{"Authorization": "Bearer local-owner-test-token"})
			require.Equal(t, http.StatusConflict, code, string(raw))
			require.Contains(t, string(raw), "hub_readonly")
			require.Equal(t, before+1, calls.Load(), "readonly peer must receive only the instance probe")
			require.Zero(t, credentials.storeCalls)
		}
	})
}

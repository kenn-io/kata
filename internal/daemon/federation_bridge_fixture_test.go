package daemon_test

import (
	"crypto/ed25519"
	"encoding/json/v2"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type bridgeHubFixture struct {
	store db.Storage
	root  projectAccessFixture
}

// newBridgeHubFixture starts a real hub daemon whose restricted project is
// federated and pinned, so a local daemon can bridge it through a catalog entry.
func newBridgeHubFixture(t *testing.T) bridgeHubFixture {
	t.Helper()
	rootStore := openReplicaServiceStore(t)
	root := newProjectAccessFixture(t, rootStore)
	_, err := rootStore.EnableProjectFederation(t.Context(), root.private.ID, "admin")
	require.NoError(t, err)
	public, private, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, rootStore.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: root.private.UID, AuthorityUID: rootStore.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
	// Relay deliveries need the hub's root attribution signer. The access
	// fixture's server has none, so serve the same store through one that does.
	signer := db.RootAttributionSigner{AuthorityUID: rootStore.InstanceUID(), PrivateKey: private}
	server := daemon.NewServer(daemon.ServerConfig{DB: rootStore, RootSigning: daemon.NewRootSigning(signer), Broadcaster: root.broadcaster, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	root.server = httptest.NewServer(server.Handler())
	t.Cleanup(root.server.Close)
	return bridgeHubFixture{store: rootStore, root: root}
}

func (h bridgeHubFixture) catalog(url string) []config.CatalogDaemonConfig {
	return []config.CatalogDaemonConfig{{Name: "selected-hub", URL: url, InstanceUID: h.store.InstanceUID(), Token: "member-test-token", AllowInsecure: true}}
}

// newBridgeLocalDaemon starts the bridging daemon with the default on-disk
// credential store under a temporary KATA_HOME.
func newBridgeLocalDaemon(t *testing.T, store db.Storage, catalog []config.CatalogDaemonConfig) projectAccessFixture {
	t.Helper()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: config.DefaultFederationCredentialStore(), FederationCatalog: catalog})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	endpoint := httptest.NewServer(server.Handler())
	t.Cleanup(endpoint.Close)
	return projectAccessFixture{store: store, server: endpoint}
}

var bridgeOwnerHeaders = map[string]string{"Authorization": "Bearer local-owner-test-token"}

func bridgeConnectBody(hubProject string, preflight bool) map[string]any {
	return map[string]any{"hub_catalog": "selected-hub", "hub_project": hubProject, "project_name": "shared-replica", "actor": "local-member", "preflight": preflight}
}

func bridgeErrorCode(t *testing.T, raw []byte) string {
	t.Helper()
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &failure), string(raw))
	return failure.Error.Code
}

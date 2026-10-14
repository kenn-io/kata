package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// A hub that rejects the catalog credential is reported as an upstream
// failure, not as the local caller's own authorization failure.
func TestFederationBridgeConnectReportsHubCredentialRejection(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	catalog := hub.catalog(hub.root.server.URL)
	catalog[0].Token = "unknown-member-test-token"
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), catalog)
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, true), bridgeOwnerHeaders)
	require.Equal(t, http.StatusBadGateway, code, string(raw))
	var failure struct {
		Error struct {
			Code string         `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &failure))
	require.Equal(t, "hub_credential_rejected", failure.Error.Code)
	require.EqualValues(t, http.StatusForbidden, failure.Error.Data["hub_status"])
}

// A hub project the account cannot see is also an upstream answer: the hub's
// 404 must not read as the local bridge route being missing.
func TestFederationBridgeConnectReportsMissingHubProject(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	local := newBridgeLocalDaemon(t, openReplicaServiceStore(t), hub.catalog(hub.root.server.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody("no-such-hub-project", true), bridgeOwnerHeaders)
	require.Equal(t, http.StatusBadGateway, code, string(raw))
	var failure struct {
		Error struct {
			Code string         `json:"code"`
			Data map[string]any `json:"data"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &failure))
	require.Equal(t, "hub_request_rejected", failure.Error.Code)
	require.EqualValues(t, http.StatusNotFound, failure.Error.Data["hub_status"])
}

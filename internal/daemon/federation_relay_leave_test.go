package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// A relay spoke holding deliveries its hub never acknowledged cannot be left
// through the ordinary leave route. The refusal names the local-only bridge
// disconnect that discards that work, instead of a generic lifecycle error
// the operator cannot act on when the hub is gone.
func TestLeaveFederationReplicaRelayPendingPushNamesBridgeDisconnect(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	hub := newBridgeHubFixture(t)
	store := openReplicaServiceStore(t)
	local := newBridgeLocalDaemon(t, store, hub.catalog(hub.root.server.URL))
	code, _, raw := local.request(t, http.MethodPost, "/api/v1/federation/bridges", "", bridgeConnectBody(hub.root.private.Name, false), bridgeOwnerHeaders)
	require.Equal(t, http.StatusOK, code, string(raw))
	project, err := store.ProjectByUID(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Never acknowledged by the hub", Author: "local-member"})
	require.NoError(t, err)

	code, _, raw = local.request(t, http.MethodPost, fmt.Sprintf("/api/v1/federation/replicas/%d/actions/leave", project.ID), "", map[string]any{"disposition": "detach", "actor": "local-member"}, bridgeOwnerHeaders)
	require.Equal(t, http.StatusConflict, code, string(raw))
	var failure struct {
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &failure))
	require.Equal(t, "federation_lifecycle_blocked", failure.Error.Code)
	require.Contains(t, failure.Error.Hint, "kata federation bridge disconnect --project shared-replica --local-only")
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err, "a refused leave keeps the binding")
	_, found, err := config.DefaultFederationCredentialStore().FederationCredential(t.Context(), hub.root.private.UID)
	require.NoError(t, err)
	require.True(t, found, "a refused leave keeps the credential")
}

package daemon_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

func TestReplicaJoinRetainsSigningReferencesBeforeSync(t *testing.T) {
	t.Setenv("TEST_REPLICA_SIGNING_KEY", strings.Repeat("k", 64))
	credentials := newReplicaCredentialStore()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.FederationCredentials = credentials })
	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", json.RawMessage(`{"hub_url":"https://HUB.EXAMPLE:443/mount","hub_project_id":1,"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","project_name":"spoke-project","replay_horizon_event_id":1,"token":"enrollment","capabilities":"pull","actor":"example-actor","signing_key_id":"key-a","signing_key_env":"TEST_REPLICA_SIGNING_KEY"}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	stored, found, err := credentials.FederationCredential(t.Context(), "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, stored.Signing)
	require.Equal(t, "key-a", stored.Signing.KeyID)
	require.Equal(t, "https://hub.example/mount", stored.Signing.HubURL)
	// Repeating native join without new signing flags must retain the source.
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", json.RawMessage(`{"hub_url":"https://hub.example/mount","hub_project_id":1,"hub_project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX","project_name":"spoke-project","replay_horizon_event_id":1,"token":"enrollment","capabilities":"pull","actor":"example-actor"}`), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	retained, _, err := credentials.FederationCredential(t.Context(), "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	require.Equal(t, stored.Signing, retained.Signing)

}

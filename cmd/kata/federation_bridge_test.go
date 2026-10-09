package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// R9: named hub selection goes to the local daemon as a catalog name. The
// CLI never resolves or transmits that catalog's upstream user credential.
func TestFederationBridgeConnectUsesCatalogSelector(t *testing.T) {
	var posted map[string]any
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/federation/bridges", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&posted))
		writeFederationRebindJSON(t, w, map[string]any{"hub_catalog": "example-hub", "hub_url": "https://hub.example", "hub_instance_uid": "00000000000000000000000002", "hub_project_uid": "00000000000000000000000003", "hub_project_id": 7, "project_name": "shared-replica", "local_account": "local-member", "upstream_account": "member", "direction": "bidirectional", "status": "ready"})
	})
	t.Setenv("KATA_AUTHOR", "local-member")
	out, err := runFederationRebindAgainstServer(t, server.URL, "--json", "--project", "shared-replica", "federation", "bridge", "connect", "--hub-daemon", "example-hub", "--hub-project", "shared-project", "--preflight")
	require.NoError(t, err)
	require.Equal(t, "example-hub", posted["hub_catalog"])
	require.Equal(t, "local-member", posted["actor"])
	require.Equal(t, "shared-project", posted["hub_project"])
	require.Equal(t, "shared-replica", posted["project_name"])
	require.Equal(t, true, posted["preflight"])
	require.Equal(t, true, posted["serve_downstream"])
	require.NotContains(t, out, "spoke-daemon-secret")
	require.NotContains(t, posted, "token")
	require.Contains(t, out, "bidirectional")
}

func TestFederationBridgeConnectRequiresSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "local project", args: []string{"federation", "bridge", "connect", "--hub-daemon", "example-hub", "--hub-project", "shared-project"}, want: "requires --project"},
		{name: "hub daemon", args: []string{"--project", "shared-replica", "federation", "bridge", "connect", "--hub-project", "shared-project"}, want: `required flag(s) "hub-daemon"`},
		{name: "hub project", args: []string{"--project", "shared-replica", "federation", "bridge", "connect", "--hub-daemon", "example-hub"}, want: `required flag(s) "hub-project"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCmdOutput(t, nil, tc.args...)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestFederationBridgeStatusUsesLocalProject(t *testing.T) {
	var calls int
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/federation/bridges/shared-replica", r.URL.Path)
		writeFederationRebindJSON(t, w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "hub_catalog": "example-hub", "hub_url": "https://hub.example", "upstream_account": "member", "local_account": "local-member", "direction": "bidirectional", "state": "enrollment_pending", "credential_status": "enrollment_pending"})
	})
	out, err := runFederationRebindAgainstServer(t, server.URL, "--json", "--project", "shared-replica", "federation", "bridge", "status")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Contains(t, out, "enrollment_pending")
	require.Contains(t, out, "local-member")
	require.NotContains(t, out, "spoke-daemon-secret")
}

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
	require.Equal(t, false, posted["serve_downstream"], "downstream relay is an explicit opt-in")
	require.NotContains(t, out, "spoke-daemon-secret")
	require.NotContains(t, posted, "token")
	require.Contains(t, out, "bidirectional")

	require.NotContains(t, posted, "signing_key_id", "no signing source is sent unless selected")
	require.NotContains(t, posted, "signing_key_env")

	_, err = runFederationRebindAgainstServer(t, server.URL, "--json", "--project", "shared-replica", "federation", "bridge", "connect", "--hub-daemon", "example-hub", "--hub-project", "shared-project", "--serve-downstream", "--signing-key-id", "bridge-key", "--signing-key-env", "KATA_BRIDGE_SIGNING_KEY")
	require.NoError(t, err)
	require.Equal(t, true, posted["serve_downstream"])
	require.Equal(t, "bridge-key", posted["signing_key_id"])
	require.Equal(t, "KATA_BRIDGE_SIGNING_KEY", posted["signing_key_env"])
	require.NotContains(t, posted, "signing_key_file")
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

// Agent mode emits an OK line naming the bridge command, so consumers can
// rely on the first token for success without parsing a row.
func TestFederationBridgeAgentOutput(t *testing.T) {
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/bridges":
			writeFederationRebindJSON(t, w, map[string]any{"hub_catalog": "example-hub", "hub_url": "https://hub.example", "hub_instance_uid": "00000000000000000000000002", "hub_project_uid": "00000000000000000000000003", "hub_project_id": 7, "project_name": "shared-replica", "upstream_account": "member", "local_account": "local-member", "direction": "bidirectional", "status": "ready"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/federation/bridges/shared-replica":
			writeFederationRebindJSON(t, w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "hub_catalog": "example-hub", "hub_url": "https://hub.example", "upstream_account": "member", "local_account": "local-member", "direction": "bidirectional", "state": "enrollment_pending", "credential_status": "pending"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/federation/bridges/shared-replica/disconnect":
			writeFederationRebindJSON(t, w, map[string]any{"project_name": "shared-replica", "project_uid": "00000000000000000000000003", "status": "disconnected", "local_only": true, "warning": "the hub grant was not revoked"})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	out, err := runFederationRebindAgainstServer(t, server.URL, "--agent", "--project", "shared-replica", "federation", "bridge", "connect", "--hub-daemon", "example-hub", "--hub-project", "shared-project", "--preflight")
	require.NoError(t, err)
	require.Equal(t, "OK federation-bridge-connect project=shared-replica status=ready direction=bidirectional hub=example-hub upstream_account=member local_account=local-member\n", out)
	out, err = runFederationRebindAgainstServer(t, server.URL, "--agent", "--project", "shared-replica", "federation", "bridge", "status")
	require.NoError(t, err)
	require.Equal(t, "OK federation-bridge-status project=shared-replica state=enrollment_pending direction=bidirectional credential_status=pending hub=example-hub upstream_account=member local_account=local-member\n", out)
	out, err = runFederationRebindAgainstServer(t, server.URL, "--agent", "--project", "shared-replica", "federation", "bridge", "disconnect", "--local-only")
	require.NoError(t, err)
	require.Equal(t, "OK federation-bridge-disconnect project=shared-replica status=disconnected warning=\"the hub grant was not revoked\"\n", out)
}

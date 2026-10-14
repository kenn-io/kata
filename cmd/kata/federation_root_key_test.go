package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The CLI asks the selected daemon to repin one project. The previous key
// path belongs to the daemon host, so it is sent exactly as given, and the
// human output carries the consequence the daemon reported.
func TestFederationRootKeyRepinCLI(t *testing.T) {
	t.Chdir(t.TempDir())
	calls := 0
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/v1/federation/root-keys/hub-project/repin", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["preflight"])
		require.Equal(t, `C:\Kata\previous.key`, body["previous_key_file"], "a daemon-host path is never resolved on the client")
		require.Nil(t, body["strand_relays"], "the strand flag is sent only when set")
		writeFederationRebindJSON(t, w, map[string]any{"project_name": "hub-project", "project_uid": "00000000000000000000000003", "status": "ready", "method": "signed_rotation", "key_file": "/srv/kata/federation-root.key", "consequence": "Downstream relays accept the new key through the signed rotation on their next sync."})
	})
	out, err := runFederationRebindAgainstServer(t, server.URL, "--project", "hub-project", "federation", "root-key", "repin", "--preflight", "--previous-key-file", `C:\Kata\previous.key`)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Contains(t, out, "root key hub-project: ready (signed_rotation)")
	require.Contains(t, out, "key file: /srv/kata/federation-root.key")
	require.Contains(t, out, "signed rotation on their next sync")
}

func TestFederationRootKeyRepinRequiresProject(t *testing.T) {
	server := newFederationRebindCLIServer(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("repin without a project must not request %s", r.URL.Path)
	})
	_, err := runFederationRebindAgainstServer(t, server.URL, "federation", "root-key", "repin")
	require.ErrorContains(t, err, "root-key repin requires --project")
}

// --strand-relays is the explicit acceptance the daemon requires before an
// unsigned repin strands active relays; the CLI never sets it by itself.
func TestFederationRootKeyRepinStrandRelaysCLI(t *testing.T) {
	calls := 0
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, true, body["strand_relays"])
		writeFederationRebindJSON(t, w, map[string]any{"project_name": "hub-project", "project_uid": "00000000000000000000000003", "status": "repinned", "method": "unsigned_repin", "active_relay_enrollments": 2})
	})
	out, err := runFederationRebindAgainstServer(t, server.URL, "--project", "hub-project", "federation", "root-key", "repin", "--strand-relays")
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Contains(t, out, "root key hub-project: repinned (unsigned_repin)")
}

func TestFederationRootKeyRepinAgentOutput(t *testing.T) {
	server := newFederationRebindCLIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFederationRebindJSON(t, w, map[string]any{"project_name": "hub-project", "project_uid": "00000000000000000000000003", "status": "ready", "method": "unsigned_repin", "key_id": "new-key", "previous_key_id": "old-key", "creates_key_file": true, "active_relay_enrollments": 2, "key_file": "/srv/kata/federation-root.key"})
	})
	out, err := runFederationRebindAgainstServer(t, server.URL, "--agent", "--project", "hub-project", "federation", "root-key", "repin", "--preflight")
	require.NoError(t, err)
	require.Equal(t, "OK federation-root-key-repin project=hub-project status=ready method=unsigned_repin key_id=new-key previous_key_id=old-key creates_key_file=true active_relay_enrollments=2 key_file=/srv/kata/federation-root.key\n", out)
}

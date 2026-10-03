package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

// A selected credential failure must stop the client before any request.
func TestAuthFileFailureStopsClientConstruction(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", filepath.Join(home, "missing-token"))
	_, err := NewHTTPClient(t.Context(), "http://127.0.0.1:7777", Opts{})
	require.Error(t, err)
	d := resolvedForRunning(DaemonSourceServerEnv, "", remoteRunningDaemon("http://127.0.0.1:7777", true)).withGlobalAuth()
	_, err = NewHTTPClientForResolved(t.Context(), d, Opts{})
	require.Error(t, err)
}

func TestAuthFileFailureSurvivesUnrelatedMalformedTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", filepath.Join(home, "missing-token"))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("invalid = ["), 0o600))
	d := resolvedForRunning(DaemonSourceServerEnv, "", remoteRunningDaemon("http://127.0.0.1:7777", true)).withGlobalAuth()
	_, err := NewHTTPClientForResolved(t.Context(), d, Opts{})
	require.Error(t, err)
}

// The runtime explicitly identifies implicit owner auth. A configured URL
// never acquires it, even when it points at that same endpoint.
func TestPersistedOwnerTokenOnlyForMarkedLocalRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	writePrivateAuthFixture(t, filepath.Join(home, "auth-token"), "owner-secret\n")
	running := runningDaemonForLive(liveDaemon{BaseURL: "http://100.64.0.5:7777", Record: kitdaemon.RuntimeRecord{Address: "100.64.0.5:7777", Metadata: map[string]string{"config_auth_source": "persisted_file"}}})
	local := resolvedForRunning(DaemonSourceLocalRuntime, "", running).withGlobalAuth()
	require.Equal(t, "owner-secret", local.Token)
	require.Empty(t, local.WithRunning(localRunningDaemon("http://100.64.0.5:7777", "100.64.0.5:7777")).Token, "a restart into a non-implicit auth mode clears the old implicit credential")
	remote := resolvedForRunning(DaemonSourceServerEnv, "", running).withGlobalAuth()
	require.Empty(t, remote.Token)
	unmarked := resolvedForRunning(DaemonSourceLocalRuntime, "", localRunningDaemon("http://100.64.0.5:7777", "100.64.0.5:7777")).withGlobalAuth()
	require.Empty(t, unmarked.Token)
	profileHome := t.TempDir()
	writePrivateAuthFixture(t, filepath.Join(profileHome, "auth-token"), "profile-secret\n")
	profile := ResolvedDaemon{Source: DaemonSourceNamedCatalog, LocalProfile: &LocalProfileIdentity{Home: profileHome}}
	require.Equal(t, "profile-secret", profile.WithRunning(running).Token)
}

func TestPersistedOwnerTokenDoesNotReachConfiguredRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	writePrivateAuthFixture(t, filepath.Join(home, "auth-token"), "owner-secret\n")
	d := resolvedForRunning(DaemonSourceServerEnv, "", remoteRunningDaemon("https://daemon.example", true)).withGlobalAuth()
	require.Empty(t, d.Token)
	token, err := GlobalAuthCredential()
	require.NoError(t, err)
	require.Empty(t, token)
}

func writePrivateAuthFixture(t *testing.T, path, contents string) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

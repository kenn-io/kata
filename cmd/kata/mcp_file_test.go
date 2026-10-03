package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPHTTPFileCredentialPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	writePrivateCredentialFixture(t, file, " file-secret\n")
	t.Setenv("EXAMPLE_MCP_TOKEN", "env-secret")
	token, err := resolveMCPHTTPToken("127.0.0.1:8080", file, "EXAMPLE_MCP_TOKEN", false)
	require.NoError(t, err)
	require.Equal(t, "file-secret", token)
	_, err = resolveMCPHTTPToken("127.0.0.1:8080", file+"-missing", "EXAMPLE_MCP_TOKEN", false)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "env-secret")
	_, err = resolveMCPHTTPToken("", file, "", false)
	require.ErrorContains(t, err, "requires --http")
	_, err = resolveMCPHTTPToken("100.64.0.5:8080", file, "", false)
	require.ErrorContains(t, err, "--trust-private-network")
}

func TestMCPHTTPFileFlagFailsBeforeBackendDiscovery(t *testing.T) {
	deploymentHome(t)
	command := newMCPServeCmd()
	command.SetArgs([]string{"--http", "127.0.0.1:8080", "--http-token-file", filepath.Join(t.TempDir(), "missing")})
	err := command.Execute()
	require.Error(t, err)
	require.NotContains(t, err.Error(), "unknown flag")
	require.Contains(t, err.Error(), "credential")
}

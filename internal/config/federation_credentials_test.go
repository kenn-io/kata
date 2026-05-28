package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadFederationCredentialsMissingReturnsEmpty(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())

	creds, err := config.ReadFederationCredentials()

	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.NotNil(t, creds.Projects)
	assert.Empty(t, creds.Projects)
}

func TestWriteFederationCredentialRoundTrips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)

	require.NoError(t, config.WriteFederationCredential("01HZNQ7VFPK1XGD8R5MABCD4EX",
		config.FederationCredential{
			HubURL:       "http://127.0.0.1:7373",
			HubProjectID: 42,
			Token:        "secret-token",
			Capabilities: "pull,push,claim",
		}))

	path := filepath.Join(home, "credentials.toml")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"]
	assert.Equal(t, "http://127.0.0.1:7373", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, "secret-token", got.Token)
	assert.Equal(t, "pull,push,claim", got.Capabilities)
}

func TestReadFederationCredentialWithoutCapabilitiesDefaultsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "credentials.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[projects."01HZNQ7VFPK1XGD8R5MABCD4EX"]
hub_url = "http://127.0.0.1:7373"
hub_project_id = 42
token = "secret-token"
`), 0o600))

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	got := creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"]
	assert.Equal(t, "http://127.0.0.1:7373", got.HubURL)
	assert.Equal(t, int64(42), got.HubProjectID)
	assert.Equal(t, "secret-token", got.Token)
	assert.Equal(t, "", got.Capabilities)
}

func TestWriteFederationCredentialTightensExistingFileMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "credentials.toml")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	require.NoError(t, os.Chmod(path, 0o644)) //nolint:gosec // Intentionally simulates a preexisting loose credentials file.

	require.NoError(t, config.WriteFederationCredential("01HZNQ7VFPK1XGD8R5MABCD4EX",
		config.FederationCredential{
			HubURL:       "http://127.0.0.1:7373",
			HubProjectID: 42,
			Token:        "secret-token",
		}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

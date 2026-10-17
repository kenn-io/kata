package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStorageReaderRejectsGlobalParseErrors(t *testing.T) {
	for _, body := range []string{"[web]\npublic_origin =\n", "[web]\npublic_orign = \"https://daemon.example\"\n"} {
		t.Run(body, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			path, err := DaemonConfigPath()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, []byte(body), 0600))
			_, err = readStorageConfig()
			require.ErrorContains(t, err, "parse ")
		})
	}
}

// The CLI warning preflight must not need daemon-only credentials, and a
// section reader must still fail when it actually consumes those credentials.
func TestScopedConfigCredentialsAndEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	t.Setenv("KATA_SEARCH_EMBEDDINGS_DIMS", "invalid")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[auth]\ntoken_file = %q\n[tui]\nmouse = true\n", filepath.Join(home, "missing-token"))), 0600))
	var warnings bytes.Buffer
	require.NoError(t, CheckConfigFile(&warnings))
	assert.Contains(t, warnings.String(), "[search]")
	cfg, err := ReadConfigSections("tui")
	require.NoError(t, err)
	assert.True(t, cfg.TUI.Mouse)
	_, err = ReadConfigSections("search")
	require.ErrorContains(t, err, "KATA_SEARCH_EMBEDDINGS_DIMS")
	_, err = ReadAuthConfig()
	require.Error(t, err)
	_, err = ReadDaemonCatalogAndAuthPolicy()
	require.NoError(t, err)
}

func TestLocalProfileIgnoresUnusedWebConfig(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[web]\npublic_origin = \"http://daemon.example\"\n"), 0600))
	profile, err := ResolveLocalProfile(CatalogDaemonConfig{Name: "example", Local: true, Home: home, InstanceUID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "kata.db"), profile.DSN)
}

func TestClientAuthIgnoresInvalidServerProxy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_AUTH_TOKEN_FILE", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\ntoken = \"example-token\"\n[auth.proxy]\ntrusted_actor_header = \"X-Example-Actor\"\n"), 0600))
	auth, err := ReadAuthConfig()
	require.NoError(t, err)
	assert.Equal(t, "example-token", auth.Token)
	_, err = ReadDaemonConfig()
	require.ErrorContains(t, err, "auth.proxy")
}

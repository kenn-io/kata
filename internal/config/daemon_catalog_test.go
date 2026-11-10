package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Catalog credentials obey the same selected-source contract as auth and MCP:
// inline > file > environment, with no fallback from a failed selected source.
func TestCatalogClientTokenSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("EXAMPLE_CATALOG_TOKEN", " env-token \n")
	path := filepath.Join(home, "token")
	writePrivateCredentialFixture(t, path, []byte(" file-token \n"))
	empty := filepath.Join(home, "empty")
	writePrivateCredentialFixture(t, empty, []byte(" \n"))
	for _, tt := range []struct {
		name, fields, want string
		fail               bool
	}{
		{"file", `token_file = "~/token"`, "file-token", false},
		{"file before env", `token_file = "~/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "file-token", false},
		{"inline before file and env", `token = " inline-token "` + "\n" + `token_file = "/missing/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "inline-token", false},
		{"missing file", `token_file = "/missing/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "", true},
		{"empty file", `token_file = "~/empty"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "", true},
		{"relative file", `token_file = "token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "", true},
		{"directory", `token_file = "~"`, "", true},
		{"env", `token_env = "EXAMPLE_CATALOG_TOKEN"`, "env-token", false},
		{"none", "", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[daemon]]\nname = 'hub'\nurl = 'https://hub.example'\n"+tt.fields), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err, "loading a catalog must not read credentials")
			got, err := cfg.Daemons[0].ClientToken()
			if tt.fail {
				require.ErrorIs(t, err, config.ErrCredentialSource)
				assert.Contains(t, err.Error(), `daemon "hub"`)
				assert.Empty(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestCatalogClientTokenRejectsPublicFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file modes")
	}
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "token")
	require.NoError(t, os.WriteFile(path, []byte("private-token"), 0o600))
	require.NoError(t, os.Chmod(path, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(fmt.Sprintf("[[daemon]]\nname = 'hub'\nurl = 'https://hub.example'\ntoken_file = %q\n", filepath.ToSlash(path))), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	token, err := cfg.Daemons[0].ClientToken()
	require.ErrorIs(t, err, config.ErrCredentialSource)
	assert.Contains(t, err.Error(), "chmod 600")
	assert.Empty(t, token)
}

func TestCatalogClientTokenKeepsTokenEnvError(t *testing.T) {
	t.Setenv("EXAMPLE_CATALOG_EMPTY", "")
	_, err := (config.CatalogDaemonConfig{Name: "hub", TokenEnv: "EXAMPLE_CATALOG_EMPTY"}).ClientToken()
	var envErr *config.TokenEnvError
	require.True(t, errors.As(err, &envErr))
	assert.Equal(t, config.TokenEnvError{Daemon: "hub", Env: "EXAMPLE_CATALOG_EMPTY"}, *envErr)
}

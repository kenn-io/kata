package config_test

import (
	"os"
	"path/filepath"
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
	t.Setenv("EXAMPLE_CATALOG_EMPTY", "")
	path := filepath.Join(home, "token")
	writePrivateCredentialFixture(t, path, []byte(" file-token \n"))
	for _, tt := range []struct {
		name, fields, want string
		wantErr            string
	}{
		{"file before env", `token_file = "~/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "file-token", ""},
		{"inline before file and env", `token = " inline-token "` + "\n" + `token_file = "/missing/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "inline-token", ""},
		{"missing file", `token_file = "/missing/token"` + "\n" + `token_env = "EXAMPLE_CATALOG_TOKEN"`, "", "file"},
		{"unset env", `token_env = "EXAMPLE_CATALOG_EMPTY"`, "", "env"},
		{"none", "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[daemon]]\nname = 'hub'\nurl = 'https://hub.example'\n"+tt.fields), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err, "loading a catalog must not read credentials")
			got, err := cfg.Daemons[0].ClientToken()
			switch tt.wantErr {
			case "env":
				var envErr *config.TokenEnvError
				require.ErrorAs(t, err, &envErr)
				assert.Equal(t, config.TokenEnvError{Daemon: "hub", Env: "EXAMPLE_CATALOG_EMPTY"}, *envErr)
			case "file":
				require.ErrorIs(t, err, config.ErrCredentialSource)
				assert.Contains(t, err.Error(), `daemon "hub"`)
				assert.Empty(t, got)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestWebCatalogOmitsSyncOnlyHubs(t *testing.T) {
	for _, tt := range []struct {
		name, active, token, mapping, extra string
		want                                []string
	}{
		{"sync only", "", "", `credential_provider = ["example-provider"]` + "\n" + `intent = "collaborate"`, `
[[daemon]]
name = "browse"
url = "https://browse.example"
token_env = "BROWSE_TOKEN"
[[daemon]]
name = "plain"
url = "https://plain.example"
`, []string{"local", "browse", "plain"}},
		{"active hub", `active_daemon = "hub"` + "\n", "", `credential_provider = ["example-provider"]` + "\n" + `intent = "collaborate"`, "", []string{"local", "hub"}},
		{"hub with token", "", `token = "example-token"` + "\n", `actor = "user-a"`, "", []string{"local", "hub"}},
		{"only sync remote", "", "", `credential_provider = ["example-provider"]` + "\n" + `intent = "collaborate"`, "", []string{"local"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			body := tt.active + `
[[daemon]]
name = "local"
local = true
[[daemon]]
name = "hub"
url = "https://hub.example"
` + tt.token + tt.extra + `
[[federation.project]]
hub = "hub"
spoke_project = "spoke-project"
hub_project = "hub-project"
` + tt.mapping + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))
			cfg, err := config.ReadDaemonConfig()
			require.NoError(t, err)
			var names []string
			for _, daemon := range cfg.WebCatalog() {
				names = append(names, daemon.Name)
			}
			assert.Equal(t, tt.want, names)
			_, found := cfg.CatalogDaemon("hub")
			assert.True(t, found)
			if tt.extra != "" {
				assert.Len(t, cfg.Daemons, 4)
			} else {
				assert.Len(t, cfg.Daemons, 2)
			}
		})
	}
}

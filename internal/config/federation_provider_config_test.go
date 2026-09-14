package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadDaemonConfigProviderMapping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	base := `
[[daemon]]
name = "team-hub"
url = "https://hub.example/tasks"
[[federation.project]]
hub = "team-hub"
spoke_project = "spoke-project"
hub_project = "hub-project"
intent = "collaborate"
credential_provider = ["example-credential-provider", "--profile", "team space"]
`
	for _, test := range []struct {
		name, text, problem string
	}{
		{"provider owns actor", base, ""},
		{"explicit actor", base + "actor = 'user-a'\n", "actor"},
		{"missing intent", strings.ReplaceAll(base, `intent = "collaborate"`, ""), "intent"},
		{"unknown intent", strings.ReplaceAll(base, `"collaborate"`, `"admin"`), "intent"},
		{"empty command", strings.ReplaceAll(base, `"example-credential-provider"`, `" "`), "credential_provider"},
		{"empty array", strings.ReplaceAll(base, `["example-credential-provider", "--profile", "team space"]`, `[]`), "credential_provider"},
		{"catalog token", strings.ReplaceAll(base, "[[federation.project]]", "token = 'synthetic-token'\n[[federation.project]]"), "token"},
		{"catalog token env", strings.ReplaceAll(base, "[[federation.project]]", "token_env = 'EXAMPLE_TOKEN'\n[[federation.project]]"), "token"},
		{"plaintext provider", strings.ReplaceAll(base, "https://hub.example/tasks", "http://127.0.0.1:8080/tasks"), "HTTPS"},
		{"intent without provider", strings.ReplaceAll(base, `credential_provider = ["example-credential-provider", "--profile", "team space"]`, `actor = "user-a"`), "credential_provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.text), 0o600))
			cfg, err := config.ReadDaemonConfig()
			if test.problem != "" {
				require.ErrorContains(t, err, test.problem)
				return
			}
			require.NoError(t, err)
			require.Len(t, cfg.Federation.Projects, 1)
			assert.Empty(t, cfg.Federation.Projects[0].Actor)
		})
	}
}

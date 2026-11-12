package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsview"
	"go.kenn.io/kata/internal/config"
)

func TestReloadAgentsViewKeepsChangedTargetAndRefreshesCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EXAMPLE_USAGE_TOKEN", "first")
	initial := config.AgentsViewConfig{URL: "https://agentsview.example", TokenEnv: "EXAMPLE_USAGE_TOKEN"}
	resolved, err := initial.Resolve(t.Context(), false)
	require.NoError(t, err)
	service, err := agentsview.NewService(resolved, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[agentsview]\nurl = \"https://other.example\"\ntoken_file = \"/missing\"\n"), 0600))
	changed, err := reloadAgentsView(t.Context(), home, service, false)
	require.NoError(t, err)
	require.False(t, changed)
	require.True(t, service.Enabled())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[agentsview]\nurl = \"https://agentsview.example\"\ntoken_env = \"EXAMPLE_USAGE_TOKEN\"\n"), 0600))
	t.Setenv("EXAMPLE_USAGE_TOKEN", "second")
	changed, err = reloadAgentsView(t.Context(), home, service, false)
	require.NoError(t, err)
	require.True(t, changed)
}

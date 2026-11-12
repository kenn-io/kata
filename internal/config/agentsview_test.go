package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsViewConfigDisabledDoesNotResolveSecret(t *testing.T) {
	got, err := (AgentsViewConfig{TokenFile: "/missing/secret", BrowserURL: "invalid"}).Resolve(t.Context(), false)
	require.NoError(t, err)
	require.False(t, got.Enabled())
}

func TestAgentsViewConfigTargetAndCredential(t *testing.T) {
	t.Setenv("EXAMPLE_AGENTSVIEW_TOKEN", "fixture-token")
	cfg := AgentsViewConfig{URL: "https://agentsview.example/archive", BrowserURL: "https://browser.example", TokenEnv: "EXAMPLE_AGENTSVIEW_TOKEN"}
	got, err := cfg.Resolve(t.Context(), false)
	require.NoError(t, err)
	require.True(t, got.Enabled())
	require.Equal(t, "fixture-token", got.Token)
	for _, value := range []string{"http://public.example", "https://user:secret@agentsview.example", "https://agentsview.example?token=secret"} {
		cfg.URL = value
		_, err = cfg.Resolve(t.Context(), false)
		require.Error(t, err)
		require.False(t, strings.Contains(err.Error(), "secret"))
	}
	cfg.URL = "http://100.64.0.1"
	_, err = cfg.Resolve(t.Context(), true)
	require.NoError(t, err)
	cfg.TokenFile = "/missing/fixture"
	_, err = cfg.Resolve(t.Context(), true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "/missing/fixture")
}

func TestAgentsViewConfigBrowserAndFileCredential(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("fixture-token\n"), 0o600))
	cfg := AgentsViewConfig{URL: "https://agentsview.example/archive", TokenFile: file}
	got, err := cfg.Resolve(t.Context(), false)
	require.NoError(t, err)
	require.Equal(t, got.URL, got.BrowserURL)
	require.Equal(t, "fixture-token", got.Token)
	cfg.BrowserURL = "https://user:secret@browser.example"
	_, err = cfg.Resolve(t.Context(), false)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	cfg.BrowserURL = ""
	cfg.URL = "http://100.64.0.1"
	_, err = cfg.Resolve(t.Context(), false)
	require.Error(t, err)
	_, err = cfg.Resolve(t.Context(), true)
	require.NoError(t, err)
}

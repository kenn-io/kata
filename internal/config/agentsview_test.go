package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsViewConfigDisabledDoesNotResolveSecret(t *testing.T) {
	got, err := (AgentsViewConfig{TokenFile: "/missing/secret", BrowserURL: "invalid"}).Resolve(t.Context(), false)
	require.NoError(t, err)
	require.False(t, got.Enabled())
}

func TestAgentsViewRejectsInvalidHeaderCredential(t *testing.T) {
	t.Setenv("EXAMPLE_AGENTSVIEW_TOKEN", "fixture\r\nsecret")
	_, err := (AgentsViewConfig{URL: "https://agentsview.example", TokenEnv: "EXAMPLE_AGENTSVIEW_TOKEN"}).Resolve(t.Context(), false)
	require.ErrorContains(t, err, "credential")
	require.ErrorContains(t, err, "env:EXAMPLE_AGENTSVIEW_TOKEN")
	require.NotContains(t, err.Error(), "fixture")
	require.NotContains(t, err.Error(), "secret")
}

func TestAgentsViewConfigResolve(t *testing.T) {
	t.Setenv("EXAMPLE_AGENTSVIEW_TOKEN", "fixture-token")
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte("fixture-token\n"), 0o600))
	for _, tc := range []struct {
		name        string
		cfg         AgentsViewConfig
		wantBrowser string
		fail        bool
	}{
		{"environment", AgentsViewConfig{URL: "https://agentsview.example/archive", BrowserURL: "https://browser.example", TokenEnv: "EXAMPLE_AGENTSVIEW_TOKEN"}, "https://browser.example", false},
		{"file and default browser", AgentsViewConfig{URL: "https://agentsview.example/archive", TokenFile: file}, "https://agentsview.example/archive", false},
		{"public HTTP", AgentsViewConfig{URL: "http://public.example"}, "", true},
		{"private HTTP untrusted", AgentsViewConfig{URL: "http://100.64.0.1"}, "", true},
		{"userinfo", AgentsViewConfig{URL: "https://user:secret@agentsview.example"}, "", true},
		{"query", AgentsViewConfig{URL: "https://agentsview.example?token=secret"}, "", true},
		{"browser userinfo", AgentsViewConfig{URL: "https://agentsview.example", BrowserURL: "https://user:secret@browser.example"}, "", true},
		{"missing credential", AgentsViewConfig{URL: "https://agentsview.example", TokenFile: "/missing/example-token"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.Resolve(t.Context(), false)
			if tc.fail {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantBrowser, got.BrowserURL)
			require.Equal(t, "fixture-token", got.Token)
		})
	}
}

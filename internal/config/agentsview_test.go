package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentsViewConfigDisabledDoesNotResolveSecret(t *testing.T) {
	got, err := (AgentsViewConfig{TokenFile: "/missing/secret", BrowserURL: "invalid"}).Resolve(t.Context(), false)
	require.NoError(t, err)
	require.False(t, got.Enabled())
}

func TestAgentsViewCredentialSelectorsAreNotDisclosed(t *testing.T) {
	selector := "fixture-selector-secret"
	// Neither fragment of the invalid value, nor its escaped form, may be echoed.
	token := "leak-first\r\nleak-second" //nolint:gosec // synthetic invalid header fixture
	file := filepath.Join(t.TempDir(), selector)
	writePrivateCredentialFixture(t, file, []byte(token))
	t.Setenv(selector, token)
	for _, tc := range []struct {
		name         string
		cfg          AgentsViewConfig
		kind, reason string
	}{
		{"missing env", AgentsViewConfig{TokenEnv: selector + "-missing"}, "token_env", "unset or empty"},
		{"missing file", AgentsViewConfig{TokenFile: file + "-missing"}, "token_file", "cannot read"},
		{"invalid env header", AgentsViewConfig{TokenEnv: selector}, "token_env", "credential"},
		{"invalid file header", AgentsViewConfig{TokenFile: file}, "token_file", "credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.URL = "https://agentsview.example"
			_, err := tc.cfg.Resolve(t.Context(), false)
			require.ErrorIs(t, err, ErrCredentialSource)
			require.ErrorContains(t, err, tc.kind)
			require.ErrorContains(t, err, tc.reason)
			require.NotContains(t, err.Error(), selector)
			require.NotContains(t, err.Error(), "leak-first")
			require.NotContains(t, err.Error(), "leak-second")
		})
	}
}

func TestAgentsViewConfigResolve(t *testing.T) {
	t.Setenv("EXAMPLE_AGENTSVIEW_TOKEN", "fixture-token")
	file := filepath.Join(t.TempDir(), "token")
	writePrivateCredentialFixture(t, file, []byte("fixture-token\n"))
	for _, tc := range []struct {
		name        string
		cfg         AgentsViewConfig
		wantBrowser string
		fail        bool
	}{
		{"environment", AgentsViewConfig{URL: "https://agentsview.example/archive", BrowserURL: "https://browser.example", TokenEnv: "EXAMPLE_AGENTSVIEW_TOKEN"}, "https://browser.example", false},
		{"file and default browser", AgentsViewConfig{URL: "https://agentsview.example/archive", TokenFile: file}, "https://agentsview.example/archive", false},
		{"public HTTP", AgentsViewConfig{URL: "http://public.example"}, "", true},
		{"userinfo", AgentsViewConfig{URL: "https://user:secret@agentsview.example"}, "", true}, //nolint:gosec // synthetic rejected URL
		{"query", AgentsViewConfig{URL: "https://agentsview.example?token=secret"}, "", true},
		{"browser userinfo", AgentsViewConfig{URL: "https://agentsview.example", BrowserURL: "https://user:secret@browser.example"}, "", true}, //nolint:gosec // synthetic rejected URL
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

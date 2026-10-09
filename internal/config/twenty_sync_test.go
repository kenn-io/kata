package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestTwentySyncConfiguration(t *testing.T) {
	c, err := config.NormalizeTwentySyncConfig(config.TwentySyncConfig{})
	require.NoError(t, err)
	require.Equal(t, "https://api.twenty.com", c.APIOrigin)
	require.Equal(t, "https://app.twenty.com", c.WebOrigin)
	require.Equal(t, "KATA_TWENTY_TOKEN", c.TokenEnv)
	for _, origin := range []string{"https://twenty.example", "https://TWENTY.example:443/", "http://127.0.0.1:3000", "http://[::1]:3000"} {
		c, err := config.NormalizeTwentySyncConfig(config.TwentySyncConfig{APIOrigin: origin, TokenEnv: " EXAMPLE_TWENTY_TOKEN "})
		require.NoError(t, err)
		require.Equal(t, "EXAMPLE_TWENTY_TOKEN", c.TokenEnv)
		require.Equal(t, c.APIOrigin, c.WebOrigin)
	}
	c, err = config.NormalizeTwentySyncConfig(config.TwentySyncConfig{APIOrigin: "https://TWENTY.example:443/", WebOrigin: "https://ui.example/"})
	require.NoError(t, err)
	require.Equal(t, "https://twenty.example", c.APIOrigin)
	require.Equal(t, "https://ui.example", c.WebOrigin)
	for _, origin := range []string{"http://twenty.example", "http://192.168.1.2:3000", "https://user:secret@twenty.example", "https://twenty.example/rest", "https://twenty.example/?token=secret", "https://twenty.example/#secret", "https://twenty.example:0", "https://twenty.example:65536", "https://twenty.example:", "//twenty.example", "https://twenty.example/%2f", "https://twenty.example?", "https://twenty.example#"} {
		for _, c := range []config.TwentySyncConfig{{APIOrigin: origin}, {WebOrigin: origin}} {
			_, err := config.NormalizeTwentySyncConfig(c)
			require.Error(t, err, origin)
			require.NotContains(t, err.Error(), "secret")
		}
	}
	for _, name := range []string{"1TOKEN", "TOKEN-NAME", "TOKEN=secret", "TOKEN NAME"} {
		_, err := config.NormalizeTwentySyncConfig(config.TwentySyncConfig{TokenEnv: name})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestTwentyDaemonConfigLoading(t *testing.T) {
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{"", true},
		{"[twenty_sync]\napi_origin='https://twenty.example'\nweb_origin='https://ui.example'\ntoken_env='EXAMPLE_TWENTY_TOKEN'", true},
		{"[twenty_sync]\ntoken='secret'", false},
		{"[twenty_sync]\napi_origin='http://twenty.example'", false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.body), 0600))
			c, err := config.ReadDaemonConfigForHome(home)
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.body == "" {
				require.Equal(t, "https://api.twenty.com", c.TwentySync.APIOrigin)
			} else {
				require.Equal(t, "https://ui.example", c.TwentySync.WebOrigin)
			}
		})
	}
}

func TestTwentyLocalProfileCredentials(t *testing.T) {
	for _, name := range []string{"KATA_TWENTY_TOKEN", "EXAMPLE_TWENTY_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "example-token")
			c, err := config.NormalizeTwentySyncConfig(config.TwentySyncConfig{TokenEnv: name})
			require.NoError(t, err)
			env, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{TwentySync: c}}, true)
			require.NoError(t, err)
			require.Contains(t, env, name+"=example-token")
		})
	}
	for _, name := range []string{"KATA_AUTH_TOKEN", "PORT", "HTTP_PROXY", "PGPASSWORD"} {
		_, err := config.LocalProfileEnvironment(config.LocalProfileConfig{Home: t.TempDir(), Config: &config.DaemonConfig{TwentySync: config.TwentySyncConfig{TokenEnv: name}}}, true)
		require.Error(t, err, name)
	}
}

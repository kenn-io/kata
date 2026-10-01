package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestPlaneSyncConfig(t *testing.T) {
	got, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{})
	require.NoError(t, err)
	require.Equal(t, "KATA_PLANE_TOKEN", got.TokenEnv)
	require.Equal(t, "https://api.plane.so", got.APIOrigin)
	require.Equal(t, "https://app.plane.so", got.WebOrigin)
	for _, origin := range []string{"https://plane.example", "https://PLANE.example:443/", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		got, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{APIOrigin: origin, TokenEnv: " EXAMPLE_PLANE_TOKEN "})
		require.NoError(t, err, origin)
		require.Equal(t, "EXAMPLE_PLANE_TOKEN", got.TokenEnv)
		require.Equal(t, got.APIOrigin, got.WebOrigin)
	}
	for _, origin := range []string{"http://plane.example", "http://192.168.1.2", "https://person:secret@plane.example", "https://plane.example/api", "https://plane.example/?token=secret", "https://plane.example/#secret", "https://plane.example:0", "https://plane.example:65536", "https://plane.example:", "//plane.example", "https://plane.example/%2f", "https://plane.example?", "https://plane.example#"} {
		_, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{APIOrigin: origin})
		require.Error(t, err, origin)
		require.NotContains(t, err.Error(), "secret")
	}
	for _, name := range []string{"1TOKEN", "TOKEN-NAME", "TOKEN=secret", "TOKEN NAME"} {
		_, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{TokenEnv: name})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
}

func TestReadDaemonConfigPlane(t *testing.T) {
	for _, body := range []string{"", "[plane_sync]\napi_origin='https://plane.example'\nweb_origin='https://app.example'\ntoken_env='EXAMPLE_PLANE_TOKEN'", "[plane_sync]\ntoken='secret'", "[plane_sync]\napi_origin='http://plane.example'"} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))
			got, err := config.ReadDaemonConfigForHome(home)
			if body == "" {
				require.NoError(t, err)
				require.Equal(t, "https://api.plane.so", got.PlaneSync.APIOrigin)
			} else if strings.Contains(body, "web_origin") {
				require.NoError(t, err)
				require.Equal(t, "https://app.example", got.PlaneSync.WebOrigin)
			} else {
				require.Error(t, err)
			}
		})
	}
}

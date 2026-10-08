package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestTickTickTokenSelector(t *testing.T) {
	c, err := config.NormalizeTickTickSyncConfig(config.TickTickSyncConfig{})
	require.NoError(t, err)
	require.Equal(t, "KATA_TICKTICK_TOKEN", c.TokenEnv)
	c, err = config.NormalizeTickTickSyncConfig(config.TickTickSyncConfig{TokenEnv: " EXAMPLE_TOKEN "})
	require.NoError(t, err)
	require.Equal(t, "EXAMPLE_TOKEN", c.TokenEnv)
	for _, name := range []string{"bad-name", "9TOKEN", "TOKEN\nOTHER"} {
		_, err = config.NormalizeTickTickSyncConfig(config.TickTickSyncConfig{TokenEnv: name})
		require.Error(t, err)
	}
}

func TestReadDaemonConfigTickTick(t *testing.T) {
	for _, body := range []string{"", "[ticktick_sync]\ntoken_env='EXAMPLE_TOKEN'", "[ticktick_sync]\ntoken='secret'", "[ticktick_sync]\ntoken_env='bad-name'"} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0600))
			got, err := config.ReadDaemonConfigForHome(home)
			switch body {
			case "":
				require.NoError(t, err)
				require.Equal(t, "KATA_TICKTICK_TOKEN", got.TickTickSync.TokenEnv)
			case "[ticktick_sync]\ntoken_env='EXAMPLE_TOKEN'":
				require.NoError(t, err)
				require.Equal(t, "EXAMPLE_TOKEN", got.TickTickSync.TokenEnv)
			default:
				require.Error(t, err)
			}
		})
	}
}

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestNotionSyncConfig(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		invalid     bool
	}{
		{"", "KATA_NOTION_TOKEN", false}, {" EXAMPLE_NOTION_TOKEN ", "EXAMPLE_NOTION_TOKEN", false},
		{"1TOKEN", "", true}, {"TOKEN-NAME", "", true}, {"TOKEN=secret", "", true}, {"TOKEN NAME", "", true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := config.NormalizeNotionSyncConfig(config.NotionSyncConfig{TokenEnv: tc.input})
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.TokenEnv)
		})
	}
}
func TestReadDaemonConfigNotion(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		invalid    bool
	}{
		{"", "KATA_NOTION_TOKEN", false},
		{"[notion_sync]\ntoken_env = ' EXAMPLE_NOTION_TOKEN '", "EXAMPLE_NOTION_TOKEN", false},
		{"[notion_sync]\ntoken_env = 'BAD-NAME'", "", true},
		{"[notion_sync]\ntoken = 'secret'", "", true},
	} {
		t.Run(tc.text, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.text), 0600))
			got, err := config.ReadDaemonConfig()
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got.NotionSync.TokenEnv)
		})
	}
}

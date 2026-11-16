package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutostartPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	for _, test := range []struct {
		content string
		enabled bool
	}{{"", true}, {"autostart = true\n", true}, {"autostart = false\n[auth]\ntoken_env = 'MISSING_SUPERVISOR_TOKEN'\n", false}} {
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.content), 0600))
		enabled, path, err := ReadAutostartEnabled()
		require.NoError(t, err)
		require.Equal(t, test.enabled, enabled)
		require.Equal(t, filepath.Join(home, "config.toml"), path)
	}
}

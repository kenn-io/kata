package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSocketActivationConfiguration(t *testing.T) {
	for _, mode := range []string{"launchd:", "launchd", "systemd:api", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("socket_activation = \""+mode+"\"\n"), 0600))
			_, err := ReadDaemonConfigForHome(home)
			require.Error(t, err)
		})
	}
}

func TestSocketActivationMalformedPolicyNeverAuthorizesControl(t *testing.T) {
	for _, contents := range []string{
		"listen =\nsocket_activation = \"systemd\"\n",
		"listen =\n\"socket_\\u0061ctivation\" = \"systemd\"\n",
		"socket_activation = \"systemd\"\nlisten =\n",
	} {
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))
		require.Error(t, RequireUnmanagedDaemonHome(t.Context(), home))
	}
}

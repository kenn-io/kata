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

func TestSocketActivationMalformedUnrelatedConfigAllowsNativeControl(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("listen =\n[auth]\ntoken = \"unused\"\n"), 0o600))
	require.NoError(t, RequireUnmanagedDaemonHome(t.Context(), home))
}

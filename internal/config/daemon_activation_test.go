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
	for name, contents := range map[string]string{
		"malformed before assignment": "listen =\nsocket_activation = \"systemd\"\n",
		"escaped assignment":          "listen =\n\"socket_\\u0061ctivation\" = \"systemd\"\n",
		"malformed after assignment":  "socket_activation = \"systemd\"\nlisten =\n",
		"invalid scalar":              "socket_activation = 1\n",
		"table":                       "[socket_activation]\nmode = \"systemd\"\n",
		"array table":                 "[[socket_activation]]\nmode = \"systemd\"\n",
		"escaped table":               "[\"socket_\\u0061ctivation\"]\nmode = \"systemd\"\n",
		"case variant":                "listen =\nSOCKET_ACTIVATION = \"systemd\"\n",
		"case variant table":          "listen =\n[Socket_Activation]\nmode = \"systemd\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(contents), 0o600))
			require.Error(t, RequireUnmanagedDaemonHome(t.Context(), home))
		})
	}
}

func TestSocketActivationMalformedUnrelatedConfigAllowsNativeControl(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("listen =\n[auth]\ntoken = \"unused\"\n"), 0o600))
	require.NoError(t, RequireUnmanagedDaemonHome(t.Context(), home))
}

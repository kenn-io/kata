package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestReadDaemonConfig_Missing(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Listen)
}

func TestReadDaemonConfig_ReadsListen(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte(`listen = "100.64.0.5:7777"`+"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "100.64.0.5:7777", cfg.Listen)
}

func TestReadDaemonConfig_TrimsWhitespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte(`listen = "  127.0.0.1:7777  "`+"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", cfg.Listen)
}

func TestReadDaemonConfig_ReadsTUIMouse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[tui]\nmouse = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.TUI.Mouse)
}

func TestReadDaemonConfig_ThrottleDefaultsEnabled(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Close.Throttle.ThrottleEnabled(),
		"absent [close.throttle] must default to enabled")
}

func TestReadDaemonConfig_ThrottleDisabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[close.throttle]\nenabled = false\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Close.Throttle.ThrottleEnabled())
}

func TestReadDaemonConfig_ThrottleExplicitlyEnabled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[close.throttle]\nenabled = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Close.Throttle.ThrottleEnabled())
}

func TestReadDaemonConfig_RejectsMalformed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte(`listen = `+"\n"), 0o600)) // unterminated

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config.toml")
}

func TestReadDaemonConfig_ReadsAuthToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"abc-123\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "abc-123", cfg.Auth.Token)
}

func TestReadDaemonConfig_ReadsAuthTrustPrivateNetwork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntrust_private_network = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.TrustPrivateNetwork)
}

func TestReadDaemonConfig_AuthTrustPrivateNetworkEnvOverridesTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntrust_private_network = false\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.TrustPrivateNetwork)
}

func TestReadDaemonConfig_AuthTokenEnvOverridesTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"from-toml\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.Auth.Token,
		"KATA_AUTH_TOKEN must override config.toml")
}

func TestReadDaemonConfig_AuthTokenEnvWorksWithoutTOML(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "from-env")

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "from-env", cfg.Auth.Token)
}

func TestReadDaemonConfig_AuthTokenAbsent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "")
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Auth.Token)
}

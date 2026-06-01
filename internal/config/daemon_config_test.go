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

func TestReadDaemonConfig_ReadsDaemonCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "remote"

[[daemon]]
name = "local"
local = true

[[daemon]]
name = "remote"
url = "https://kata.example.test"
token = "target-token"
allow_insecure = true
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 2)
	assert.Equal(t, "remote", cfg.ActiveDaemon)
	assert.Equal(t, config.CatalogDaemonConfig{Name: "local", Local: true}, cfg.Daemons[0])
	assert.Equal(t, config.CatalogDaemonConfig{
		Name:          "remote",
		URL:           "https://kata.example.test",
		Token:         "target-token",
		AllowInsecure: true,
	}, cfg.Daemons[1])
}

func TestReadDaemonConfig_RejectsDuplicateDaemonNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "prod"
local = true

[[daemon]]
name = "prod"
url = "https://kata.example.test"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate daemon name")
	assert.Contains(t, err.Error(), "prod")
}

func TestReadDaemonConfig_RejectsDaemonWithLocalAndURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "ambiguous"
local = true
url = "https://kata.example.test"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local")
	assert.Contains(t, err.Error(), "url")
}

func TestReadDaemonConfig_RejectsActiveDaemonMissingFromCatalog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "missing"

[[daemon]]
name = "local"
local = true
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "active_daemon")
	assert.Contains(t, err.Error(), "missing")
}

func TestReadDaemonConfig_TrimsDaemonFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "  remote  "

[[daemon]]
name = "  remote  "
url = "  https://kata.example.test  "
token = "  target-token  "
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 1)
	assert.Equal(t, "remote", cfg.ActiveDaemon)
	assert.Equal(t, "remote", cfg.Daemons[0].Name)
	assert.Equal(t, "https://kata.example.test", cfg.Daemons[0].URL)
	assert.Equal(t, "target-token", cfg.Daemons[0].Token)
}

func TestReadDaemonConfig_PreservesDaemonTokenEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_WORK_TOKEN", "secret-from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "work"
url = "https://kata.example.test"
token_env = "KATA_WORK_TOKEN"
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 1)
	assert.Empty(t, cfg.Daemons[0].Token)
	assert.Equal(t, "KATA_WORK_TOKEN", cfg.Daemons[0].TokenEnv,
		"token_env resolution is deferred until a daemon target is selected")
}

func TestReadDaemonConfig_AllowsInactiveDaemonTokenEnvUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_WORK_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "local"

[[daemon]]
name = "local"
local = true

[[daemon]]
name = "work"
url = "https://kata.example.test"
token_env = "KATA_WORK_TOKEN"
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 2)
	assert.Equal(t, "KATA_WORK_TOKEN", cfg.Daemons[1].TokenEnv)
	assert.Empty(t, cfg.Daemons[1].Token)
}

func TestReadDaemonConfig_RejectsDaemonTokenAndTokenEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_WORK_TOKEN", "secret-from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "work"
url = "https://kata.example.test"
token = "inline-token"
token_env = "KATA_WORK_TOKEN"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "token and token_env are mutually exclusive")
	assert.Contains(t, err.Error(), "token")
	assert.Contains(t, err.Error(), "token_env")
}

func TestReadDaemonConfig_TrimsDaemonTokenEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_WORK_TOKEN", "secret-from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "work"
url = "https://kata.example.test"
token_env = "  KATA_WORK_TOKEN  "
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Len(t, cfg.Daemons, 1)
	assert.Equal(t, "KATA_WORK_TOKEN", cfg.Daemons[0].TokenEnv)
	assert.Empty(t, cfg.Daemons[0].Token)
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

func TestReadDaemonConfig_ReadsRequireTokenIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\nrequire_token_identity = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.RequireTokenIdentity)
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

func TestReadDaemonConfig_AutostartIdentityModeKeepsConfiguredBootstrapToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "client-db-token")
	t.Setenv("KATA_AUTOSTART", "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"bootstrap-token\"\nrequire_token_identity = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "bootstrap-token", cfg.Auth.Token)
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

func TestReadDaemonConfig_AuthProxy(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")

	dir := t.TempDir()
	t.Setenv("KATA_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	body := `
[auth]
token = "tok"

[auth.proxy]
trusted_actor_header = "X-Kata-Actor"
trusted_proxy_listeners = ["unix:///run/kata/proxy.sock", "100.64.0.5:7777"]
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, "X-Kata-Actor", cfg.Auth.Proxy.TrustedActorHeader)
	require.Equal(t,
		[]string{"unix:///run/kata/proxy.sock", "100.64.0.5:7777"},
		cfg.Auth.Proxy.TrustedProxyListeners)
}

func TestApplyDaemonConfigEnv_AuthProxyHeader(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "X-Env-Actor")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	// Listeners are set in TOML so the resolved config is complete; this
	// test asserts only that the env header beats the TOML header.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_actor_header = \"X-Toml-Actor\"\ntrusted_proxy_listeners = [\"unix:///s\"]\n"), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t, "X-Env-Actor", cfg.Auth.Proxy.TrustedActorHeader,
		"KATA_TRUSTED_ACTOR_HEADER must override config.toml")
}

func TestReadDaemonConfig_RejectsHeaderWithoutListeners(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_actor_header = \"X-Kata-Actor\"\n"), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err,
		"header set without listeners must reject at config load, not silently no-op")
	assert.Contains(t, err.Error(), "trusted_proxy_listeners",
		"error must name the missing key so the operator can fix it")
}

func TestReadDaemonConfig_RejectsEnvOnlyHeaderWithoutListeners(t *testing.T) {
	// No config.toml. Env supplies a header but no listeners. The
	// missing-file path used to short-circuit before validation, so this
	// would silently start with proxy attribution effectively off — the
	// exact misconfig the file-present path rejects. Both paths must
	// agree.
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "X-Kata-Actor")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")
	t.Setenv("KATA_HOME", t.TempDir())

	_, err := config.ReadDaemonConfig()
	require.Error(t, err,
		"env-only header without listeners must reject same as TOML-only")
	assert.Contains(t, err.Error(), "trusted_proxy_listeners")
}

func TestReadDaemonConfig_AcceptsListenersWithoutHeader(t *testing.T) {
	// listeners without a header is dead config: the mode is off (no
	// principal overwrite ever happens), so it has no security impact.
	// Accept it silently so partial configs in the safe direction don't
	// block daemon start.
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_proxy_listeners = [\"unix:///s\"]\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Auth.Proxy.TrustedActorHeader)
	assert.Equal(t, []string{"unix:///s"}, cfg.Auth.Proxy.TrustedProxyListeners)
}

func TestReadDaemonConfig_EnvCompletesPartialTOMLProxy(t *testing.T) {
	// TOML supplies only the header (would be rejected alone); env adds
	// listeners. Validation runs after env merge, so this is valid.
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_ACTOR_HEADER", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS", "unix:///s")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_actor_header = \"X-Kata-Actor\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "X-Kata-Actor", cfg.Auth.Proxy.TrustedActorHeader)
	assert.Equal(t, []string{"unix:///s"}, cfg.Auth.Proxy.TrustedProxyListeners)
}

func TestReadDaemonConfig_ReadsStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"postgres://db/kata\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/kata", cfg.Storage.DSN)
}

func TestReadDaemonConfig_TrimsStorageDSN(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"  postgres://db/kata  \"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Equal(t, "postgres://db/kata", cfg.Storage.DSN)
}

func TestReadDaemonConfig_EmptyStorageDSNIsZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\ndsn = \"\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Storage.DSN)
}

func TestReadDaemonConfig_StorageRejectsUnknownKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[storage]\nfoo = \"bar\"\n"), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "meta.Undecoded() must catch typo'd storage keys")
	assert.Contains(t, err.Error(), "storage.foo")
}

func TestApplyDaemonConfigEnv_AuthProxyListeners(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_TRUSTED_PROXY_LISTENERS",
		"unix:///s1 , 100.64.0.5:7777 ,, ")

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth.proxy]\ntrusted_proxy_listeners = [\"unix:///toml-only\"]\n"), 0o600))
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	require.Equal(t,
		[]string{"unix:///s1", "100.64.0.5:7777"},
		cfg.Auth.Proxy.TrustedProxyListeners,
		"KATA_TRUSTED_PROXY_LISTENERS must split on commas, trim, drop empties, override config.toml")
}

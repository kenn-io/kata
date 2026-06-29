package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestReadDaemonConfig_GitHubSyncDefaultsTokenEnv(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)

	assert.Equal(t, "KATA_GITHUB_TOKEN", cfg.GitHubSync.TokenEnvName())
	assert.Equal(t, "github.com", cfg.GitHubSync.TokenHostName())
	assert.Empty(t, cfg.GitHubSync.Apps)
}

func TestReadDaemonConfig_ReadsGitHubSyncConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_GITHUB_TOKEN", "secret-from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[github_sync]
token_env = "EXAMPLE_GITHUB_TOKEN"
token_host = " GitHub.Example "

[[github_sync.app]]
host = " GitHub.Example "
owner = " Example-Owner "
app_id = 12345
installation_id = 67890
private_key_path = " /secure/example-app.pem "
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)

	assert.Equal(t, "EXAMPLE_GITHUB_TOKEN", cfg.GitHubSync.TokenEnvName())
	assert.Equal(t, "github.example", cfg.GitHubSync.TokenHostName())
	require.Len(t, cfg.GitHubSync.Apps, 1)
	app := cfg.GitHubSync.Apps[0]
	assert.Equal(t, "github.example", app.Host)
	assert.Equal(t, "Example-Owner", app.Owner)
	assert.Equal(t, int64(12345), app.AppID)
	assert.Equal(t, int64(67890), app.InstallationID)
	assert.Equal(t, "/secure/example-app.pem", app.PrivateKeyPath)
}

func TestReadDaemonConfig_GitHubSyncRejectsDuplicateApps(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[github_sync.app]]
host = "github.example"
owner = "Example-Owner"
app_id = 1
installation_id = 2
private_key_path = "/secure/one.pem"

[[github_sync.app]]
host = "GITHUB.EXAMPLE"
owner = "example-owner"
app_id = 3
installation_id = 4
private_key_path = "/secure/two.pem"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github_sync.app")
	assert.Contains(t, err.Error(), "duplicate")
	assert.Contains(t, err.Error(), "github.example")
	assert.Contains(t, err.Error(), "example-owner")
}

func TestReadDaemonConfig_GitHubSyncRejectsIncompleteApp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[github_sync.app]]
owner = "example-owner"
app_id = 1
installation_id = 2
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github_sync.app")
	assert.Contains(t, err.Error(), "private_key_path")
}

func TestReadDaemonConfig_GitHubSyncRejectsNegativeAppIDs(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{
			name: "app id",
			config: `
[[github_sync.app]]
owner = "example-owner"
app_id = -1
installation_id = 2
private_key_path = "/secure/example.pem"
`,
			wantErr: "app_id",
		},
		{
			name: "installation id",
			config: `
[[github_sync.app]]
owner = "example-owner"
app_id = 1
installation_id = -2
private_key_path = "/secure/example.pem"
`,
			wantErr: "installation_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tt.config), 0o600))

			_, err := config.ReadDaemonConfig()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "github_sync.app")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestReadDaemonConfig_ThrottleDefaultsDisabled(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Close.Throttle.ThrottleEnabled(),
		"absent [close.throttle] must default sibling-burst throttling off")
}

func TestReadDaemonConfig_ThrottleExplicitlyDisabled(t *testing.T) {
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

func TestReadDaemonConfig_ThrottleWindow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[close.throttle]\nenabled = true\nwindow = \"2m\"\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	window, err := cfg.Close.Throttle.ThrottleWindow()
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, window)
}

func TestReadDaemonConfig_ThrottleWindowRejectsInvalidDuration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[close.throttle]\nwindow = \"eventually\"\n"), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "close.throttle.window")
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

func TestReadDaemonConfig_ReadsAllowUnauthenticatedPrivateNetworkWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\nallow_unauthenticated_private_network_writes = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.AllowUnauthenticatedPrivateNetworkWrites)
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

func TestReadDaemonConfig_AllowUnauthenticatedPrivateNetworkWritesEnvOverridesTOML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES", "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\nallow_unauthenticated_private_network_writes = false\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.AllowUnauthenticatedPrivateNetworkWrites)
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

func TestReadDaemonConfig_SearchEmbeddingsValid(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://localhost:11434/v1"
model = "nomic-embed-text"
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Search.Embeddings.Enabled(),
		"base_url + model present must enable embeddings")
	assert.Equal(t, "http://localhost:11434/v1", cfg.Search.Embeddings.BaseURL)
	assert.Equal(t, "nomic-embed-text", cfg.Search.Embeddings.Model)
}

func TestReadDaemonConfig_SearchEmbeddingsDisabledWhenAbsent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Search.Embeddings.Enabled(),
		"absent [search.embeddings] must leave semantic search disabled")
}

func TestReadDaemonConfig_SearchEmbeddingsRejectsBaseURLWithoutModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://localhost:11434/v1"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "base_url without model is a partial config and must reject")
	assert.Contains(t, err.Error(), "base_url")
	assert.Contains(t, err.Error(), "model")
}

func TestReadDaemonConfig_SearchEmbeddingsRejectsModelWithoutBaseURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
model = "nomic-embed-text"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "model without base_url is a partial config and must reject")
	assert.Contains(t, err.Error(), "base_url")
	assert.Contains(t, err.Error(), "model")
}

func TestReadDaemonConfig_SearchEmbeddingsRejectsAPIKeyAndAPIKeyEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://localhost:11434/v1"
model = "m"
api_key = "x"
api_key_env = "Y"
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "api_key and api_key_env are mutually exclusive")
	assert.Contains(t, err.Error(), "api_key")
	assert.Contains(t, err.Error(), "api_key_env")
}

func TestReadDaemonConfig_SearchEmbeddingsRejectsNegativeDims(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://localhost:11434/v1"
model = "m"
dims = -1
`), 0o600))

	_, err := config.ReadDaemonConfig()
	require.Error(t, err, "negative dims must reject")
	assert.Contains(t, err.Error(), "dims")
}

func TestReadDaemonConfig_SearchEmbeddingsResolvedAPIKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_EMBED_KEY", "secret-from-env")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[search.embeddings]
base_url = "http://localhost:11434/v1"
model = "m"
api_key_env = "KATA_EMBED_KEY"
`), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.Search.Embeddings.APIKey)
	assert.Equal(t, "secret-from-env", cfg.Search.Embeddings.ResolvedAPIKey(),
		"api_key_env must resolve from the environment")
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

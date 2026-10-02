package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

func deploymentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))
	for _, name := range []string{"KATA_DSN", "KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_AUTH_TOKEN_FILE", "KATA_AUTOSTART", "KATA_LISTEN", "KATA_WEB_LISTEN", "KATA_WEB_PUBLIC_ORIGIN", "PORT", "KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES"} {
		t.Setenv(name, "")
	}
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	return home
}

func TestDeploymentStatusReportsConfigSources(t *testing.T) {
	resetFlags(t)
	deploymentHome(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{PID: os.Getpid(), Network: "tcp", Address: "127.0.0.1:7777", Metadata: map[string]string{"config_listen_source": "KATA_LISTEN", "config_auth_source": "persisted_file", "db_path": "database", "unrelated": "ignored"}})
	require.NoError(t, err)
	out := executeRoot(t, newRootCmd(), "--json", "daemon", "status")
	var response struct {
		Daemons []struct {
			ConfigSources map[string]string `json:"config_sources"`
		} `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(out, &response))
	require.Len(t, response.Daemons, 1)
	require.Equal(t, map[string]string{"listen": "KATA_LISTEN", "auth": "persisted_file"}, response.Daemons[0].ConfigSources)
}

func TestDeploymentConcurrentTokenCreation(t *testing.T) {
	home := deploymentHome(t)
	var workers sync.WaitGroup
	var tokens [8]string
	var failures [8]error
	for i := range tokens {
		workers.Go(func() {
			result, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
			failures[i] = err
			if err == nil {
				tokens[i] = result.Config.Auth.Token
			}
		})
	}
	workers.Wait()
	for i := range tokens {
		require.NoError(t, failures[i])
		require.Equal(t, tokens[0], tokens[i])
		require.NotEmpty(t, tokens[i])
	}
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".auth-token-")
	}
}

func TestDeploymentNoAutoTokenStopsCreation(t *testing.T) {
	home := deploymentHome(t)
	_, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, true)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "auth-token"))
	require.True(t, os.IsNotExist(err))
}

func TestDeploymentNoAutoTokenFlagReachesStartAndRestart(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			resetFlags(t)
			deploymentHome(t)
			old := startDetachedDaemon
			t.Cleanup(func() { startDetachedDaemon = old })
			var disabled bool
			startDetachedDaemon = func(_ context.Context, _ string, _ bool, noAutoToken bool, _ bool) (daemonStartOutput, error) {
				disabled = noAutoToken
				return daemonStartOutput{Action: "started"}, nil
			}
			_, _, err := executeRootCapture(t, t.Context(), "daemon", action, "--listen", "127.0.0.1:7777", "--no-auto-token")
			require.NoError(t, err)
			require.True(t, disabled)
		})
	}
}

// Contract: explicit non-loopback service startup mints one private persisted
// owner token and reuses it on restart; legacy inline env still wins.
func TestDeploymentPreflightMintsAndReusesToken(t *testing.T) {
	home := deploymentHome(t)
	first, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
	require.NoError(t, err)
	require.Len(t, first.Config.Auth.Token, 64)
	data, err := os.ReadFile(filepath.Join(home, "auth-token")) //nolint:gosec // G304: test-owned home
	require.NoError(t, err)
	require.Equal(t, first.Config.Auth.Token+"\n", string(data))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(home, "auth-token"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	second, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
	require.NoError(t, err)
	require.Equal(t, first.Config.Auth.Token, second.Config.Auth.Token)
	t.Setenv("KATA_AUTH_TOKEN", "legacy-secret")
	third, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
	require.NoError(t, err)
	require.Equal(t, "legacy-secret", third.Config.Auth.Token)
}

func TestDeploymentPersistedTokenModeMatrix(t *testing.T) {
	for _, mode := range []string{"loopback", "readonly", "tokenless", "autostart", "proxy", "opt-out-reuse"} {
		t.Run(mode, func(t *testing.T) {
			home := deploymentHome(t)
			writePrivateCredentialFixture(t, filepath.Join(home, "auth-token"), "owner-secret\n")
			listen, readonly := "100.64.0.5:7777", false
			switch mode {
			case "loopback":
				listen = "127.0.0.1:7777"
			case "readonly":
				readonly = true
			case "tokenless":
				t.Setenv("KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES", "1")
			case "autostart":
				t.Setenv("KATA_AUTOSTART", "1")
			case "proxy":
				listen = "127.0.0.1:7777"
				t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth.proxy]\ntrusted_actor_header = \"X-Actor\"\ntrusted_proxy_listeners = [\"127.0.0.1:7777\"]\n"), 0o600))
			case "opt-out-reuse":
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\nauto_token = false\n"), 0o600))
			}
			result, err := preflightDaemonStartup(t.Context(), listen, readonly, false)
			if mode == "autostart" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if mode == "opt-out-reuse" {
				require.Equal(t, "owner-secret", result.Config.Auth.Token)
			} else {
				require.Empty(t, result.Config.Auth.Token)
			}
		})
	}
}

func TestDeploymentNoTrustFailsBeforeTokenCreation(t *testing.T) {
	home := deploymentHome(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	_, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(home, "auth-token"))
	require.True(t, os.IsNotExist(err))
}

func TestDeploymentMissingInterfaceFailsBeforeTokenCreation(t *testing.T) {
	home := deploymentHome(t)
	t.Setenv("KATA_LISTEN", "iface:missing-interface-example:7777")
	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.ErrorContains(t, err, "interface")
	_, err = os.Stat(filepath.Join(home, "auth-token"))
	require.True(t, os.IsNotExist(err))
}

func TestDeploymentPreflightReportsSafeSources(t *testing.T) {
	deploymentHome(t)
	t.Setenv("KATA_LISTEN", "100.64.0.5:7777")
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
	result, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, "KATA_LISTEN", result.Config.Sources["listen"])
	require.Equal(t, "KATA_WEB_PUBLIC_ORIGIN", result.Config.Sources["web_public_origin"])
	require.Equal(t, "generated_file", result.Config.Sources["auth"])
	for _, source := range result.Config.Sources {
		require.NotContains(t, source, result.Config.Auth.Token)
	}
}

// Only effective non-loopback listeners may select implicit owner auth.
func TestDeploymentLoopbackIgnoresUnusedWebListener(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:7777", "127.0.0.1:0", "[::1]:7777", "localhost:7777"} {
		for _, persisted := range []bool{false, true} {
			t.Run(listen+"/persisted="+strconv.FormatBool(persisted), func(t *testing.T) {
				home := deploymentHome(t)
				if persisted {
					writePrivateCredentialFixture(t, filepath.Join(home, "auth-token"), "unused-owner-secret\n")
				}
				cfg := &config.DaemonConfig{Auth: config.AuthConfig{TrustPrivateNetwork: true}}
				cfg.Web.Listen = "100.64.0.5:7778"
				err := prepareDaemonAuth(cfg, kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: listen}, false, false, home)
				require.NoError(t, err)
				require.Empty(t, cfg.Auth.Token)
				if !persisted {
					_, err := os.Stat(filepath.Join(home, "auth-token"))
					require.True(t, os.IsNotExist(err))
				}
			})
		}
	}
}

func TestDeploymentUnixBrowserPersistsAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a shared TCP endpoint")
	}
	home := deploymentHome(t)
	t.Setenv("KATA_WEB_LISTEN", "100.64.0.5:7778")
	result, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, kitdaemon.NetworkUnix, result.Endpoint.Network)
	require.Len(t, result.Config.Auth.Token, 64)
	token, err := config.ReadPersistedAuthToken(home)
	require.NoError(t, err)
	require.Equal(t, result.Config.Auth.Token, token)
}

func TestDeploymentExternalHTTPSOriginMintsLoginToken(t *testing.T) {
	deploymentHome(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")

	result, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, "generated_file", result.Config.Auth.Source)
	require.NotEmpty(t, result.Config.Auth.Token)
}

func TestDeploymentExternalHTTPSOriginRequiresAuthWhenAutoTokenDisabled(t *testing.T) {
	home := deploymentHome(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[auth]\nauto_token = false\n"), 0o600))

	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.ErrorContains(t, err, "web.public_origin")
}

func TestDeploymentExternalHTTPSOriginKeepsNonLoopbackBackendTrust(t *testing.T) {
	deploymentHome(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	if runtime.GOOS == "windows" {
		// Windows serves the web UI on the shared daemon TCP endpoint.
		t.Setenv("KATA_LISTEN", "100.64.0.5:7777")
	} else {
		t.Setenv("KATA_WEB_LISTEN", "100.64.0.5:7778")
	}
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")

	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.ErrorContains(t, err, "trust_private_network")
}

func TestDeploymentEmptyTokenFileEnvironmentPreservesCredential(t *testing.T) {
	for _, value := range []string{"", " \t "} {
		t.Run(value, func(t *testing.T) {
			home := deploymentHome(t)
			file := filepath.Join(home, "mounted-token")
			writePrivateCredentialFixture(t, file, "operator-secret\n")
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
				[]byte(fmt.Sprintf("[auth]\ntoken_file = %q\n", file)), 0o600))
			t.Setenv("KATA_AUTH_TOKEN_FILE", value)
			startup, err := preflightDaemonStartup(t.Context(), "100.64.0.5:7777", false, false)
			require.NoError(t, err)
			require.Equal(t, "operator-secret", startup.Config.Auth.Token)
			auth, err := config.ReadAuthConfig()
			require.NoError(t, err)
			require.Equal(t, "operator-secret", auth.Token)
			_, err = os.Lstat(filepath.Join(home, "auth-token"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestDeploymentDanglingTokenSymlinkFails(t *testing.T) {
	home := deploymentHome(t)
	if err := os.Symlink(filepath.Join(home, "missing-token"), filepath.Join(home, "auth-token")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink privilege unavailable: %v", err)
		}
		require.NoError(t, err)
	}
	t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
	_, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.ErrorIs(t, err, config.ErrCredentialSource)
	// A winner at the destination must supply a token, including during publication.
	token, err := mintDaemonToken(home)
	require.ErrorIs(t, err, config.ErrCredentialSource)
	require.Empty(t, token)
}

func TestDeploymentProxyListenersWithoutHeaderRequireToken(t *testing.T) {
	for _, autoToken := range []bool{true, false} {
		t.Run(strconv.FormatBool(autoToken), func(t *testing.T) {
			home := deploymentHome(t)
			t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
				[]byte(fmt.Sprintf("[auth]\nauto_token = %t\n[auth.proxy]\ntrusted_proxy_listeners = [\"127.0.0.1:7777\"]\n", autoToken)), 0o600))
			startup, err := preflightDaemonStartup(t.Context(), "127.0.0.1:7777", false, false)
			if !autoToken {
				require.ErrorContains(t, err, "web.public_origin")
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, startup.Config.Auth.Token)
		})
	}
}

func TestDeploymentProxyListenerMustMatchBoundAddress(t *testing.T) {
	for _, tc := range []struct {
		name, listen, trusted string
		browser, proxy        bool
	}{
		{name: "shared ephemeral", listen: "127.0.0.1:0", trusted: "127.0.0.1:0"},
		{name: "browser ephemeral", browser: true, listen: "127.0.0.1:0", trusted: "127.0.0.1:0"},
		{name: "padded port", browser: true, listen: "127.0.0.1:07777", trusted: "127.0.0.1:07777"},
		{name: "expanded IPv6", browser: true, listen: "[0:0:0:0:0:0:0:1]:7777", trusted: "[0:0:0:0:0:0:0:1]:7777"},
		{name: "canonical trusted IPv6", browser: true, listen: "[0:0:0:0:0:0:0:1]:7777", trusted: "[::1]:7777", proxy: true},
	} {
		for _, noAutoToken := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-auto-token=%t", tc.name, noAutoToken), func(t *testing.T) {
				if tc.browser && runtime.GOOS == "windows" {
					t.Skip("Windows uses the shared TCP listener")
				}
				home := deploymentHome(t)
				t.Setenv("KATA_WEB_PUBLIC_ORIGIN", "https://daemon.example")
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
					[]byte(fmt.Sprintf("[auth.proxy]\ntrusted_actor_header = \"X-Actor\"\ntrusted_proxy_listeners = [%q]\n", tc.trusted)), 0o600))
				listen := tc.listen
				if tc.browser {
					listen = ""
					t.Setenv("KATA_WEB_LISTEN", tc.listen)
				}
				startup, err := preflightDaemonStartup(t.Context(), listen, false, noAutoToken)
				if tc.proxy {
					require.NoError(t, err)
					require.Empty(t, startup.Config.Auth.Token)
				} else if noAutoToken {
					require.ErrorContains(t, err, "web.public_origin")
				} else {
					require.NoError(t, err)
					require.NotEmpty(t, startup.Config.Auth.Token)
				}
			})
		}
	}
}

func TestDeploymentExternalHTTPSOnlySkipsTokenForUsableBrowserAuth(t *testing.T) {
	for _, tc := range []struct {
		name             string
		endpoint         kitdaemon.Endpoint
		webListen        string
		actorHeader      string
		proxyListeners   []string
		tokenlessWrites  bool
		checkBackendAuth bool
		wantToken        bool
		wantAuthError    bool
	}{
		{name: "listeners without actor header", proxyListeners: []string{"127.0.0.1:7777"}, wantToken: true},
		{name: "actor header on different listener", actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:7778"}, wantToken: true},
		{name: "tokenless private writes without proxy cannot authenticate browser", tokenlessWrites: true, wantAuthError: true},
		{name: "matching trusted proxy listener", actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:7777"}},
		{name: "matching trusted proxy authenticates tokenless private writes", actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:7777"}, tokenlessWrites: true},
		{name: "matching trusted proxy on non-loopback API listener still requires owner token", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "100.64.0.5:7777"}, actorHeader: "X-Example-Actor", proxyListeners: []string{"100.64.0.5:7777"}, checkBackendAuth: true, wantToken: true},
		{name: "matching trusted proxy on separate unix browser listener does not require backend token", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}, webListen: "100.64.0.5:7778", actorHeader: "X-Example-Actor", proxyListeners: []string{"100.64.0.5:7778"}},
		{name: "matching unix daemon browser listener", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}, webListen: "127.0.0.1:7777", actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:7777"}},
		{name: "default unix browser listener is ephemeral", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}, actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:7777"}, wantToken: true},
		{name: "unix browser listener on port zero is not stable", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}, webListen: "127.0.0.1:0", actorHeader: "X-Example-Actor", proxyListeners: []string{"127.0.0.1:0"}, wantToken: true},
		{name: "unix browser hostname is not an exact listener address", endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/kata.sock"}, webListen: "localhost:7777", actorHeader: "X-Example-Actor", proxyListeners: []string{"localhost:7777"}, wantToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := deploymentHome(t)
			cfg := &config.DaemonConfig{}
			cfg.Web.PublicOrigin = "https://daemon.example"
			cfg.Web.Listen = tc.webListen
			cfg.Auth.TrustPrivateNetwork = true
			cfg.Auth.AllowUnauthenticatedPrivateNetworkWrites = tc.tokenlessWrites
			cfg.Auth.Proxy.TrustedActorHeader = tc.actorHeader
			cfg.Auth.Proxy.TrustedProxyListeners = tc.proxyListeners

			endpoint := tc.endpoint
			if endpoint.Network == "" {
				endpoint = kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:7777"}
			}
			err := prepareDaemonAuth(cfg, endpoint, false, false, home)
			if tc.wantAuthError {
				require.ErrorContains(t, err, "browser listener")
				require.Empty(t, cfg.Auth.Token)
				return
			}
			require.NoError(t, err)
			if tc.wantToken {
				require.NotEmpty(t, cfg.Auth.Token)
				require.Equal(t, "generated_file", cfg.Auth.Source)
			} else {
				require.Empty(t, cfg.Auth.Token)
			}
			if tc.checkBackendAuth {
				require.NoError(t, daemon.CheckAuthStartup(endpoint.Address, cfg.Auth, false))
			}
		})
	}
}

// A dedicated browser listener authenticated by a configured trusted proxy may
// use tab-scoped proxy sessions with a Unix daemon API and no backend token.
func TestDeploymentPreflightAllowsTrustedProxyBrowserListener(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the shared TCP listener")
	}

	home := deploymentHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[web]
listen = "100.64.0.5:7778"
public_origin = "https://daemon.example"

[auth]
trust_private_network = true

[auth.proxy]
trusted_actor_header = "X-Actor"
trusted_proxy_listeners = ["100.64.0.5:7778"]
`), 0o600))

	startup, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, kitdaemon.NetworkUnix, startup.Endpoint.Network)
	require.Empty(t, startup.Config.Auth.Token)
}

func TestDeploymentSharedIPv6ProxyEndpointMatchesCanonicalTrustedListener(t *testing.T) {
	home := deploymentHome(t)
	listen := "[fd00:0:0:0:0:0:0:5]:7777"
	t.Setenv("KATA_LISTEN", listen)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[web]
public_origin = "https://daemon.example"

[auth]
trust_private_network = true
allow_unauthenticated_private_network_writes = true

[auth.proxy]
trusted_actor_header = "X-Actor"
trusted_proxy_listeners = ["[fd00::5]:7777"]
`), 0o600))

	startup, err := preflightDaemonStartup(t.Context(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, listen, startup.Endpoint.Address)
	require.Empty(t, startup.Config.Auth.Token)
	require.True(t, (daemon.WebEndpoint{Endpoint: startup.Endpoint}).AllowsTrustedProxySession(startup.Config.Auth))
}

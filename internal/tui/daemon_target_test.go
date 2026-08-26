package tui

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
)

func TestDaemonTargetsFromConfigIncludesConfiguredEntries(t *testing.T) {
	daemons := []config.CatalogDaemonConfig{
		{Name: "local", Local: true},
		{Name: "shared", URL: "http://100.64.0.5:7777", TokenEnv: "KATA_SHARED_TOKEN", AllowInsecure: true}, //nolint:gosec // env var name, not a credential
	}

	targets := daemonTargetsFromConfig(daemons)

	require.Len(t, targets, 2)
	assert.Equal(t, daemonTarget{Name: "local", Local: true}, targets[0])
	assert.Equal(t, daemonTarget{ //nolint:gosec // env var name, not a credential
		Name:     "shared",
		URL:      "http://100.64.0.5:7777",
		TokenEnv: "KATA_SHARED_TOKEN",
		resolved: clientpkg.ResolvedDaemon{AllowInsecure: true},
	}, targets[1])
}

func TestActiveDaemonTargetUsesConfiguredActive(t *testing.T) {
	targets := []daemonTarget{
		{Name: "local", Local: true},
		{Name: "shared", URL: "https://kata.example.test"},
	}

	target, ok := activeDaemonTarget(targets, "shared")

	require.True(t, ok)
	assert.Equal(t, "shared", target.Name)
}

func TestDaemonTargetDisplayPrefersName(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{Name: "shared", URL: "https://kata.example.test:9443"})

	assert.Equal(t, "shared", got)
}

func TestDaemonTargetDisplayFallsBackToHostPort(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{URL: "https://kata.example.test:9443"})

	assert.Equal(t, "kata.example.test:9443", got)
}

func TestDaemonTargetDisplayLocalFallback(t *testing.T) {
	got := daemonTargetDisplay(daemonTarget{Local: true})

	assert.Equal(t, "local", got)
}

func TestConnectDaemonTargetLocalUsesLocalOnlyEnsurePath(t *testing.T) {
	oldEnsure := ensureResolvedForTUI
	oldEnsureNamed := ensureResolvedNamedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		ensureResolvedForTUI = oldEnsure
		ensureResolvedNamedForTUI = oldEnsureNamed
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	var ensured bool
	ensureResolvedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		t.Fatal("explicit local target must not honor remote-aware EnsureRunning")
		return clientpkg.ResolvedDaemon{}, nil
	}
	ensureResolvedNamedForTUI = func(_ context.Context, name string) (clientpkg.ResolvedDaemon, error) {
		ensured = true
		require.Equal(t, "local", name)
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: name, BaseURL: "http://kata.invalid",
		}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := connectDaemonTarget(context.Background(), daemonTarget{Name: "local", Local: true})

	require.NoError(t, err)
	assert.True(t, ensured, "explicit local daemon must use local-only ensure path")
	assert.Equal(t, "http://kata.invalid", conn.endpoint)
	assert.Equal(t, "local", daemonTargetDisplay(conn.target))
	assert.Equal(t, viewEmpty, conn.init.view)
}

func TestConnectResolvedRemoteUsesPathFreeBoot(t *testing.T) {
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})

	newHTTPClientForTUI = func(
		context.Context,
		string,
		daemonTarget,
		clientOptsKind,
	) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		t.Fatal("remote daemon must not use start_path-capable boot")
		return bootInit{}, nil
	}
	var called bool
	bootResolveScopePathFreeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		called = true
		return bootInit{view: viewProjects}, nil
	}

	conn, err := connectResolvedDaemonTarget(t.Context(),
		daemonTarget{Name: "shared", URL: "https://daemon.example"},
		"https://daemon.example")

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, viewProjects, conn.init.view)
}

func TestConnectResolvedImplicitLoopbackUsesLocalBoot(t *testing.T) {
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})

	newHTTPClientForTUI = func(
		context.Context,
		string,
		daemonTarget,
		clientOptsKind,
	) (*http.Client, error) {
		return &http.Client{}, nil
	}
	var called bool
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		called = true
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	bootResolveScopePathFreeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		t.Fatal("implicit loopback daemon shares the client filesystem")
		return bootInit{}, nil
	}
	endpoint := "http://127.0.0.1:7777"

	conn, err := connectResolvedDaemonTarget(t.Context(), implicitDaemonTarget(endpoint), endpoint)

	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, viewEmpty, conn.init.view)
}

func TestConnectImplicitConfiguredLoopbackUsesPathFreeBoot(t *testing.T) {
	for _, source := range []string{"environment", "workspace config"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_SERVER", "")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/ping" {
					http.NotFound(w, r)
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"ok":      true,
					"service": "kata",
					"version": "test",
				}))
			}))
			t.Cleanup(srv.Close)

			if source == "environment" {
				t.Setenv("KATA_SERVER", srv.URL)
			} else {
				workspace := t.TempDir()
				t.Chdir(workspace)
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"),
					[]byte("version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n"),
					0o600))
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
					[]byte("version = 1\n\n[server]\nurl = \""+srv.URL+"\"\n"),
					0o600))
			}

			oldEnsure := ensureResolvedForTUI
			oldNewClient := newHTTPClientForTUI
			oldBootScope := bootResolveScopeForTUI
			oldPathFreeBootScope := bootResolveScopePathFreeForTUI
			t.Cleanup(func() {
				ensureResolvedForTUI = oldEnsure
				newHTTPClientForTUI = oldNewClient
				bootResolveScopeForTUI = oldBootScope
				bootResolveScopePathFreeForTUI = oldPathFreeBootScope
			})

			ensureResolvedForTUI = clientpkg.EnsureResolvedInWorkspace
			newHTTPClientForTUI = func(
				context.Context,
				string,
				daemonTarget,
				clientOptsKind,
			) (*http.Client, error) {
				return &http.Client{}, nil
			}
			bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
				t.Fatal("configured loopback daemon must not use start_path-capable boot")
				return bootInit{}, nil
			}
			var called bool
			bootResolveScopePathFreeForTUI = func(context.Context, *Client, string) (bootInit, error) {
				called = true
				return bootInit{view: viewProjects}, nil
			}

			conn, err := connectImplicitDaemonTarget(t.Context(), "", false)

			require.NoError(t, err)
			assert.True(t, called)
			assert.Equal(t, srv.URL, conn.endpoint)
			assert.Equal(t, viewProjects, conn.init.view)
		})
	}
}

func TestConnectImplicitWorkspaceRemoteThreadsPlaintextAuthOptions(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "workspace-token")
	cwd := t.TempDir()
	t.Chdir(cwd)
	workspace := t.TempDir()
	require.NoError(t, config.WriteProjectConfig(workspace, "spoke-project"))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte(`version = 1

[server]
url = "http://daemon.internal:7777"
allow_insecure = true
`), 0o600))

	oldEnsure := ensureResolvedForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		ensureResolvedForTUI = oldEnsure
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})
	ensureResolvedForTUI = func(_ context.Context, gotWorkspace string) (clientpkg.ResolvedDaemon, error) {
		assert.Equal(t, workspace, gotWorkspace)
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceLocalConfig, BaseURL: "http://daemon.internal:7777",
			Token: "workspace-token", AllowInsecure: true,
		}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		t.Fatal("workspace-configured remote must use path-free boot")
		return bootInit{}, nil
	}
	bootResolveScopePathFreeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewProjects}, nil
	}

	conn, err := connectImplicitDaemonTarget(t.Context(), workspace, false)

	require.NoError(t, err)
	assert.True(t, conn.target.resolved.AllowInsecure)
	require.NotNil(t, conn.api)
	require.NotNil(t, conn.sseHC)
}

func TestBootDaemonConnectionWithoutActiveKeepsRemoteAwareEnsureRunningPath(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldEnsure := ensureResolvedForTUI
	oldEnsureLocal := ensureLocalRunningTargetForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		ensureResolvedForTUI = oldEnsure
		ensureLocalRunningTargetForTUI = oldEnsureLocal
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{}, nil
	}
	t.Chdir(t.TempDir())
	workspace := t.TempDir()
	var ensuredWorkspace string
	ensureResolvedForTUI = func(_ context.Context, workspace string) (clientpkg.ResolvedDaemon, error) {
		ensuredWorkspace = workspace
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceLocalRuntime, BaseURL: "http://kata.invalid",
		}, nil
	}
	ensureLocalRunningTargetForTUI = func(context.Context) (clientpkg.RunningDaemon, error) {
		t.Fatal("implicit default boot must keep existing remote-aware EnsureRunning behavior")
		return clientpkg.RunningDaemon{}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	var resolvedWorkspace string
	bootResolveScopeForTUI = func(_ context.Context, _ *Client, start string) (bootInit, error) {
		resolvedWorkspace = start
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := bootDaemonConnection(
		context.Background(), Options{Workspace: workspace},
	)

	require.NoError(t, err)
	assert.Equal(t, workspace, ensuredWorkspace)
	assert.Equal(t, workspace, resolvedWorkspace)
	assert.Equal(t, "http://kata.invalid", conn.endpoint)
	assert.Equal(t, "local", daemonTargetDisplay(conn.target))
	assert.Equal(t, viewEmpty, conn.init.view)
}

func TestBootDaemonConnectionDefinitiveTargetSkipsCWDResolution(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldEnsure := ensureResolvedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		ensureResolvedForTUI = oldEnsure
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})
	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{}, nil
	}
	ensureResolvedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		return clientpkg.ResolvedDaemon{Source: clientpkg.DaemonSourceLocalRuntime, BaseURL: clientpkg.UnixBase}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{}, errors.New("malformed cwd binding")
	}

	for name, opts := range map[string]Options{
		"qualified ref":    {InitialIssueRef: "other-project#abc4"},
		"explicit project": {ProjectName: "other-project"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := bootDaemonConnection(t.Context(), opts)
			require.NoError(t, err)
		})
	}
}

func TestBootDaemonConnectionOptionsDaemonNameOverridesActiveDaemon(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldConnect := connectDaemonTargetForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		connectDaemonTargetForTUI = oldConnect
	})

	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{
			ActiveDaemon: "active",
			Daemons: []config.CatalogDaemonConfig{
				{Name: "active", URL: "https://active.example"},
				{Name: "selected", URL: "https://selected.example"},
			},
		}, nil
	}
	var got daemonTarget
	connectDaemonTargetForTUI = func(_ context.Context, target daemonTarget) (daemonConnection, error) {
		got = target
		return daemonConnection{target: target}, nil
	}
	workspace := t.TempDir()

	conn, err := bootDaemonConnection(context.Background(), Options{
		DaemonName:  "selected",
		ProjectName: "project-b",
		Workspace:   workspace,
	})

	require.NoError(t, err)
	assert.Equal(t, "selected", got.Name)
	assert.Equal(t, workspace, got.workspaceStart)
	assert.True(t, got.skipInitialScope)
	assert.Equal(t, "selected", conn.target.Name)
	require.Len(t, conn.catalog, 2)
	for _, target := range conn.catalog {
		assert.Equal(t, workspace, target.workspaceStart)
		assert.True(t, target.skipInitialScope)
	}
}

func TestBootDaemonConnectionWithoutActiveLabelsImplicitRemoteEndpoint(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldEnsure := ensureResolvedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		ensureResolvedForTUI = oldEnsure
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})

	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{}, nil
	}
	ensureResolvedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceServerEnv, BaseURL: "https://daemon.example:7777",
		}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	bootResolveScopePathFreeForTUI = bootResolveScopeForTUI

	conn, err := bootDaemonConnection(context.Background(), Options{})

	require.NoError(t, err)
	assert.False(t, conn.target.Local)
	assert.Equal(t, "https://daemon.example:7777", conn.target.URL)
	assert.Equal(t, "daemon.example:7777", daemonTargetDisplay(conn.target))
}

func TestResolvedImplicitRemoteTargetCarriesEnvAllowInsecure(t *testing.T) {
	srv := startTUIPingServer(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", srv.URL)
	t.Setenv("KATA_ALLOW_INSECURE", "1")

	resolved, ok, err := clientpkg.ResolveRemoteDaemon(t.Context(), "")

	require.NoError(t, err)
	require.True(t, ok)
	assert.True(t, resolved.AllowInsecure)
}

func TestResolvedImplicitRemoteTargetCarriesGlobalAuthToken(t *testing.T) {
	srv := startTUIPingServer(t)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", srv.URL)
	t.Setenv("KATA_ALLOW_INSECURE", "1")
	t.Setenv("KATA_AUTH_TOKEN", "global-token")

	resolved, ok, err := clientpkg.ResolveRemoteDaemon(t.Context(), "")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "global-token", resolved.Token)
}

func TestResolvedImplicitRemoteTargetEnvTokenOverridesAuthConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "client-db-token")
	t.Setenv("KATA_AUTOSTART", "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"bootstrap-token\"\nrequire_token_identity = true\n"), 0o600))

	srv := startTUIPingServer(t)
	t.Setenv("KATA_SERVER", srv.URL)
	resolved, ok, err := clientpkg.ResolveRemoteDaemon(t.Context(), "")

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "client-db-token", resolved.Token)
}

func TestConnectResolvedImplicitRemoteUsesEnvTokenForHTTPClient(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "client-db-token")
	t.Setenv("KATA_AUTOSTART", "1")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\ntoken = \"bootstrap-token\"\nrequire_token_identity = true\n"), 0o600))

	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		bootResolveScopeForTUI = oldBootScope
	})

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ00","schema_version":14}`))
	}))
	t.Cleanup(srv.Close)

	bootResolveScopeForTUI = func(ctx context.Context, c *Client, _ string) (bootInit, error) {
		_, err := c.GetInstance(ctx)
		if err != nil {
			return bootInit{}, err
		}
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	target := implicitDaemonTarget(srv.URL)
	target.resolved = clientpkg.ResolvedDaemon{
		Source: clientpkg.DaemonSourceServerEnv, BaseURL: srv.URL, Token: "client-db-token",
	}
	conn, err := connectResolvedDaemonTarget(t.Context(), target, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, "Bearer client-db-token", gotAuth)
	assert.Equal(t, "client-db-token", conn.target.resolved.Token)
}

func TestNewHTTPClientForTUIResolvedImplicitRemoteHonorsTrustPrivateNetwork(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	endpoint := "http://100.64.0.5:7777"
	target := implicitDaemonTarget(endpoint)
	target.resolved = clientpkg.ResolvedDaemon{
		Source: clientpkg.DaemonSourceServerEnv, BaseURL: endpoint,
		Token: "global-token", TrustPrivateNetwork: true,
	}
	require.Equal(t, "global-token", target.resolved.Token)
	require.False(t, target.resolved.AllowInsecure)

	_, err := newHTTPClientForTUI(t.Context(), endpoint, target, clientOptsNormal)

	require.NoError(t, err)
}

func TestNewHTTPClientForTUILocalFallsBackToGlobalAuth(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	hc, err := newHTTPClientForTUI(t.Context(), srv.URL, daemonTarget{
		Local: true,
		resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceLocalRuntime, BaseURL: srv.URL, Token: "global-token",
		},
	}, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestNewHTTPClientForTUIExplicitLocalBypassesActiveDaemonAuth(t *testing.T) {
	originalEnsureResolvedNamed := ensureResolvedNamedForTUI
	t.Cleanup(func() { ensureResolvedNamedForTUI = originalEnsureResolvedNamed })
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_REMOTE_TOKEN", "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	ensureResolvedNamedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "local",
			BaseURL: srv.URL, Token: "global-token",
		}, nil
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "remote"

[auth]
token = "global-token"

[[daemon]]
name = "local"
local = true

[[daemon]]
name = "remote"
url = "`+srv.URL+`"
token_env = "KATA_REMOTE_TOKEN"
`), 0o600))

	hc, err := newHTTPClientForTUI(t.Context(), srv.URL,
		daemonTarget{Name: "local", Local: true, resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "local",
			BaseURL: srv.URL, Token: "global-token",
		}}, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestNewHTTPClientForTUIExplicitRemoteAuthTokenEnvOverridesCatalogToken(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_AUTH_TOKEN", "env-token")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	hc, err := newHTTPClientForTUI(t.Context(), srv.URL,
		daemonTarget{Name: "shared", URL: srv.URL, resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "shared",
			BaseURL: srv.URL, Token: "env-token",
		}}, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer env-token", gotAuth)
}

func TestNewHTTPClientForTUIImplicitRemoteFallsBackToGlobalAuth(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	target := implicitDaemonTarget(srv.URL)
	target.resolved = clientpkg.ResolvedDaemon{
		Source: clientpkg.DaemonSourceServerEnv, BaseURL: srv.URL, Token: "global-token",
	}
	hc, err := newHTTPClientForTUI(t.Context(), srv.URL, target, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestNewHTTPClientForTUIImplicitRemoteAnchorsCompatibilityToWorkspace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_MISSING_TOKEN", "")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
active_daemon = "shared"

[auth]
token = "global-token"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_MISSING_TOKEN"
`), 0o600))

	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte(
		"version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n",
	), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(
		"version = 1\n\n[server]\nurl = \""+srv.URL+"\"\n",
	), 0o600))
	unrelatedCWD := t.TempDir()
	t.Chdir(unrelatedCWD)

	target := implicitDaemonTarget(srv.URL)
	target.workspaceStart = workspace
	target.resolved = clientpkg.ResolvedDaemon{
		Source: clientpkg.DaemonSourceLocalConfig, BaseURL: srv.URL, Token: "global-token",
	}
	hc, err := newHTTPClientForTUI(t.Context(), srv.URL, target, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/protected", nil)
	require.NoError(t, err)
	resp, err := hc.Do(req) //nolint:gosec // request targets the test's own server
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestConnectDaemonTargetRemoteUsesPerDaemonAuth(t *testing.T) {
	oldEnsureResolvedNamed := ensureResolvedNamedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		ensureResolvedNamedForTUI = oldEnsureResolvedNamed
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})

	target := daemonTarget{Name: "shared", URL: "http://daemon.internal:7777"}
	var gotNormal, gotSSE daemonTarget
	ensureResolvedNamedForTUI = func(_ context.Context, name string) (clientpkg.ResolvedDaemon, error) {
		require.Equal(t, target.Name, name)
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: name,
			BaseURL: target.URL, Token: "tok", AllowInsecure: true,
		}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, target daemonTarget, kind clientOptsKind) (*http.Client, error) {
		if kind == clientOptsNormal {
			gotNormal = target
		} else {
			gotSSE = target
		}
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	bootResolveScopePathFreeForTUI = bootResolveScopeForTUI

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "http://daemon.internal:7777", conn.endpoint)
	expected := target
	expected.resolved = clientpkg.ResolvedDaemon{
		Source: clientpkg.DaemonSourceNamedCatalog, Name: "shared",
		BaseURL: target.URL, Token: "tok", AllowInsecure: true,
	}
	assert.Equal(t, expected, gotNormal)
	assert.Equal(t, expected, gotSSE)
	assert.Equal(t, "shared", conn.target.Name)
}

func TestConnectDaemonTargetCarriesResolvedProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("HUB_TOKEN_ENV", "catalog-token")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "service": "kata", "version": "test",
		}))
	}))
	t.Cleanup(srv.Close)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "hub-project-daemon"
url = "`+srv.URL+`"
token_env = "HUB_TOKEN_ENV"
`), 0o600))

	oldNewClient := newHTTPClientForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		newHTTPClientForTUI = oldNewClient
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})
	newHTTPClientForTUI = func(
		context.Context, string, daemonTarget, clientOptsKind,
	) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopePathFreeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewProjects}, nil
	}

	conn, err := connectDaemonTarget(t.Context(), daemonTarget{ //nolint:gosec // Fake credential environment name verifies token snapshotting.
		Name: "hub-project-daemon", URL: srv.URL, TokenEnv: "HUB_TOKEN_ENV",
	})
	require.NoError(t, err)

	assert.Equal(t, "catalog-token", conn.target.resolved.Token)
	assert.Equal(t, clientpkg.DaemonSourceNamedCatalog, conn.target.resolved.Source)
	t.Setenv("HUB_TOKEN_ENV", "rotated-token")
	assert.Equal(t, "catalog-token", conn.target.resolved.Token)
}

func TestConnectDaemonTargetRemoteResolvesTokenEnvOnUse(t *testing.T) {
	oldEnsureResolvedNamed := ensureResolvedNamedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		ensureResolvedNamedForTUI = oldEnsureResolvedNamed
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})
	t.Setenv("KATA_WORK_TOKEN", "secret-from-env")

	target := daemonTarget{Name: "shared", URL: "https://daemon.example", TokenEnv: "KATA_WORK_TOKEN"} //nolint:gosec // env var name, not a credential
	var gotNormal, gotSSE daemonTarget
	ensureResolvedNamedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "shared",
			BaseURL: target.URL, Token: "secret-from-env",
		}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, target daemonTarget, kind clientOptsKind) (*http.Client, error) {
		if kind == clientOptsNormal {
			gotNormal = target
		} else {
			gotSSE = target
		}
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	bootResolveScopePathFreeForTUI = bootResolveScopeForTUI

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "secret-from-env", gotNormal.resolved.Token)
	assert.Equal(t, "secret-from-env", gotSSE.resolved.Token)
	assert.Equal(t, "secret-from-env", conn.target.resolved.Token)
}

func TestConnectDaemonTargetRemoteAuthTokenEnvOverridesUnsetTokenEnv(t *testing.T) {
	oldEnsureResolvedNamed := ensureResolvedNamedForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldPathFreeBootScope := bootResolveScopePathFreeForTUI
	t.Cleanup(func() {
		ensureResolvedNamedForTUI = oldEnsureResolvedNamed
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		bootResolveScopePathFreeForTUI = oldPathFreeBootScope
	})
	t.Setenv("KATA_AUTH_TOKEN", "env-token")
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_WORK_TOKEN", "")

	target := daemonTarget{Name: "shared", URL: "https://daemon.example", TokenEnv: "KATA_WORK_TOKEN"} //nolint:gosec // env var name, not a credential
	var gotNormal, gotSSE daemonTarget
	ensureResolvedNamedForTUI = func(context.Context, string) (clientpkg.ResolvedDaemon, error) {
		return clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "shared",
			BaseURL: target.URL, Token: "env-token",
		}, nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, target daemonTarget, kind clientOptsKind) (*http.Client, error) {
		if kind == clientOptsNormal {
			gotNormal = target
		} else {
			gotSSE = target
		}
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	bootResolveScopePathFreeForTUI = bootResolveScopeForTUI

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "env-token", gotNormal.resolved.Token)
	assert.Equal(t, "env-token", gotSSE.resolved.Token)
	assert.Equal(t, "env-token", conn.target.resolved.Token)
}

func TestConnectResolvedLocalTargetRetryRefreshPreservesTargetToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	oldEnsureLocal := ensureLocalRunningTargetForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	oldRefresh := refreshLocalHTTPClientForTUI
	t.Cleanup(func() {
		ensureLocalRunningTargetForTUI = oldEnsureLocal
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
		refreshLocalHTTPClientForTUI = oldRefresh
	})

	t.Setenv("KATA_HOME", t.TempDir())
	socket := filepath.Join(t.TempDir(), "fresh-kata.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var gotAuth string
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]any{"issues": []map[string]any{}})
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ensureLocalRunningTargetForTUI = func(context.Context) (clientpkg.RunningDaemon, error) {
		return clientpkg.RunningDaemon{
			BaseURL: clientpkg.UnixBase, Address: "unix://" + socket,
			Network: "unix", Scheme: "http",
		}, nil
	}
	refreshLocalHTTPClientForTUI = func(context.Context) (*http.Client, error) {
		t.Fatal("local retry should refresh through the resolved daemon target")
		return nil, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}
	var normalCalls int
	var retryTarget daemonTarget
	newHTTPClientForTUI = func(
		ctx context.Context, _ string, target daemonTarget, kind clientOptsKind,
	) (*http.Client, error) {
		if kind == clientOptsSSE {
			return &http.Client{}, nil
		}
		normalCalls++
		if normalCalls == 1 {
			return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial unix /tmp/missing.sock: connect: no such file or directory")
			})}, nil
		}
		retryTarget = target
		return clientpkg.NewHTTPClientForResolved(ctx, target.resolved,
			clientpkg.Opts{Timeout: time.Second})
	}

	conn, err := connectResolvedDaemonTarget(t.Context(),
		daemonTarget{Name: "local-secure", Local: true, resolved: clientpkg.ResolvedDaemon{
			Source: clientpkg.DaemonSourceNamedCatalog, Name: "local-secure",
			BaseURL: clientpkg.UnixBase, Address: "unix:///tmp/stale.sock",
			Network: "unix", Scheme: "http", UnixSocket: "/tmp/stale.sock",
			Token: "target-token", AllowInsecure: true, TrustPrivateNetwork: true,
		}},
		clientpkg.UnixBase)
	require.NoError(t, err)

	_, err = conn.api.ListIssues(t.Context(), 7, ListFilter{Limit: 2001})
	require.NoError(t, err)
	assert.Equal(t, "target-token", retryTarget.resolved.Token)
	assert.Equal(t, clientpkg.DaemonSourceNamedCatalog, retryTarget.resolved.Source)
	assert.Equal(t, "local-secure", retryTarget.resolved.Name)
	assert.Equal(t, clientpkg.UnixBase, retryTarget.resolved.BaseURL)
	assert.Equal(t, "unix://"+socket, retryTarget.resolved.Address)
	assert.Equal(t, "unix", retryTarget.resolved.Network)
	assert.Equal(t, "http", retryTarget.resolved.Scheme)
	assert.Equal(t, socket, retryTarget.resolved.UnixSocket)
	assert.True(t, retryTarget.resolved.AllowInsecure)
	assert.True(t, retryTarget.resolved.TrustPrivateNetwork)
	assert.Equal(t, "Bearer target-token", gotAuth)
}

func TestConnectDaemonTargetRemoteRejectsUnsetTokenEnvOnUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_WORK_TOKEN", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[[daemon]]
name = "shared"
url = "https://daemon.example"
token_env = "KATA_WORK_TOKEN"
`), 0o600))

	_, err := connectDaemonTarget(context.Background(),
		daemonTarget{Name: "shared", URL: "https://daemon.example", TokenEnv: "KATA_WORK_TOKEN"}) //nolint:gosec // env var name, not a credential

	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_env")
	assert.Contains(t, err.Error(), "KATA_WORK_TOKEN")
}

func TestBuildRunModelCarriesDaemonMetadata(t *testing.T) {
	conn := daemonConnection{
		target:  daemonTarget{Name: "shared", URL: "https://kata.example.test"},
		catalog: []daemonTarget{{Name: "local", Local: true}, {Name: "shared", URL: "https://kata.example.test"}},
		init:    bootInit{view: viewEmpty, scope: scope{empty: true}},
	}

	m := buildRunModel(Options{}, &Client{}, conn.init, conn)

	assert.Equal(t, "shared", m.activeDaemon.Name)
	require.Len(t, m.daemonTargets, 2)
}

func TestDaemonConnectionUsesSSEHeaderTimeout(t *testing.T) {
	opts := optsForKind(clientOptsSSE)

	assert.Equal(t, clientSSEHandshakeTimeout(), opts.ResponseHeaderTimeout)
	assert.Zero(t, opts.Timeout)
}

func clientSSEHandshakeTimeout() time.Duration {
	return 10 * time.Second
}

func startTUIPingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "service": "kata", "version": "test",
		}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

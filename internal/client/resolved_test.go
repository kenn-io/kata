package client

import (
	"context"
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
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

// TestNewHTTPClientForResolvedUsesCarriedUnixSocket pins that client
// construction consumes the socket selected during resolution. Requiring a
// runtime record here would reintroduce a time-of-check/time-of-use re-scan.
func TestNewHTTPClientForResolvedUsesCarriedUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	setupKataEnv(t)
	socket := filepath.Join(t.TempDir(), "kata.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The namespace deliberately has no runtime record. The resolved value is
	// complete and must remain sufficient after runtime discovery state changes.
	resolved := ResolvedDaemon{
		Source:     DaemonSourceLocalRuntime,
		BaseURL:    UnixBase,
		UnixSocket: socket,
	}
	c, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{Timeout: time.Second})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, UnixBase+"/ready", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// TestNewHTTPClientForResolvedNamedNeedsNoURLCrossCheck pins that a named
// catalog daemon builds from its resolved provenance. The old construction
// path re-resolved the catalog entry and compared base URLs, a guard that only
// existed because the name-to-URL binding was discarded before construction.
func TestNewHTTPClientForResolvedNamedNeedsNoURLCrossCheck(t *testing.T) {
	var gotAuthorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
			return
		}
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "hub-project-daemon"
url = "`+srv.URL+`"
token = "catalog-token"
`))

	resolved, err := EnsureResolvedNamed(t.Context(), "hub-project-daemon")
	require.NoError(t, err)
	c, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "Bearer catalog-token", gotAuthorization)
}

func TestResolvedDaemonSameOriginTwoSourcesPicksHigherPriority(t *testing.T) {
	srv := startResolvableDaemon(t)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", srv.URL)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, writeRawConfig(home, `
active_daemon = "shared"

[auth]
token = "global-token"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token = "catalog-token"
`))

	resolved, err := EnsureResolvedInWorkspace(t.Context(), "")
	require.NoError(t, err)

	assert.Equal(t, DaemonSourceServerEnv, resolved.Source)
	assert.Equal(t, "global-token", resolved.Token)
	assert.True(t, resolved.ConfiguredRemote())
}

func TestNewHTTPClientForResolvedDoesNotRereadConfiguration(t *testing.T) {
	var gotAuthorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/ping" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
			return
		}
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SERVER", "")
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, writeAuthConfig(home, "resolved-token"))

	workspace := t.TempDir()
	writeNeutralWorkspaceMarker(t, workspace)
	localPath := filepath.Join(workspace, ".kata.local.toml")
	require.NoError(t, os.WriteFile(localPath,
		[]byte("version = 1\n\n[server]\nurl = \""+srv.URL+"\"\n"), 0o600))

	resolved, err := EnsureResolvedInWorkspace(t.Context(), workspace)
	require.NoError(t, err)
	require.Equal(t, DaemonSourceLocalConfig, resolved.Source)

	// Construction must consume the value above, not any state changed after it.
	t.Setenv("KATA_AUTH_TOKEN", "replacement-token")
	require.NoError(t, writeRawConfig(home, "not = valid = toml"))
	require.NoError(t, os.WriteFile(localPath,
		[]byte("[server]\nurl = \"http://elsewhere.invalid\"\nallow_insecure = true\n"), 0o600))

	c, err := NewHTTPClientForResolved(t.Context(), resolved, Opts{Timeout: time.Second})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/protected", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "Bearer resolved-token", gotAuthorization)
	assert.False(t, resolved.AllowInsecure)
}

func TestResolvedDaemonSourcesTable(t *testing.T) {
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	t.Run("server env", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", srv.URL)
		t.Setenv("KATA_ALLOW_INSECURE", "1")
		t.Setenv("KATA_AUTH_TOKEN", "")
		require.NoError(t, writeAuthConfig(home, "global-token"))

		resolved, err := EnsureResolvedInWorkspace(t.Context(), "")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceServerEnv, "global-token", true)
	})

	t.Run("workspace local config", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "")
		require.NoError(t, writeAuthConfig(home, "global-token"))
		workspace := t.TempDir()
		writeNeutralWorkspaceMarker(t, workspace)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(`
version = 1

[server]
url = "`+srv.URL+`"
token = "ignored-local-token"
allow_insecure = true
`), 0o600))

		resolved, err := EnsureResolvedInWorkspace(t.Context(), workspace)
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceLocalConfig, "global-token", true)
	})

	t.Run("active daemon entry token env", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv("KATA_SHARED_TOKEN", "catalog-env-token")
		require.NoError(t, writeRawConfig(home, `
active_daemon = "shared"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_SHARED_TOKEN"
allow_insecure = true
`))

		resolved, err := EnsureResolvedInWorkspace(t.Context(), "")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceActiveDaemon, "catalog-env-token", true)
		assert.Equal(t, "shared", resolved.Name)
	})

	t.Run("active daemon explicit env override", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "override-token")
		t.Setenv("KATA_SHARED_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
active_daemon = "shared"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_SHARED_TOKEN"
`))

		resolved, err := EnsureResolvedInWorkspace(t.Context(), "")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceActiveDaemon, "override-token", false)
	})

	t.Run("named catalog remote token env", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv("KATA_NAMED_TOKEN", "catalog-env-token")
		require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_NAMED_TOKEN"
allow_insecure = true
`))

		resolved, err := EnsureResolvedNamed(t.Context(), "shared")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceNamedCatalog, "catalog-env-token", true)
		assert.Equal(t, remoteRunningDaemon(srv.URL, true), resolved.Running())
	})

	t.Run("named catalog remote explicit env override", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "override-token")
		t.Setenv("KATA_NAMED_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_NAMED_TOKEN"
`))

		resolved, err := EnsureResolvedNamed(t.Context(), "shared")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceNamedCatalog, "override-token", false)
	})

	t.Run("named catalog local token env", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "global-token")
		t.Setenv("KATA_LOCAL_TOKEN", "local-entry-token")
		require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "local"
local = true
token_env = "KATA_LOCAL_TOKEN"
`))
		patchEnsureHooks(t, currentVersionForEnsure(), "http://127.0.0.1:27123")

		resolved, err := EnsureResolvedNamed(t.Context(), "local")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceNamedCatalog, "local-entry-token", false)
		assert.False(t, resolved.ConfiguredRemote())
	})

	t.Run("named catalog local global fallback", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
[auth]
token = "global-token"

[[daemon]]
name = "local"
local = true
`))
		patchEnsureHooks(t, currentVersionForEnsure(), "http://127.0.0.1:27123")

		resolved, err := EnsureResolvedNamed(t.Context(), "local")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceNamedCatalog, "global-token", false)
	})

	t.Run("local runtime", func(t *testing.T) {
		home := setupKataEnv(t)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "")
		require.NoError(t, writeAuthConfig(home, "global-token"))
		_, address := startMockDaemonPing(t, map[string]any{
			"ok": true, "service": "kata", "version": currentVersionForEnsure(),
		})
		require.NoError(t, writeRuntimeRecord(t, home, address))

		resolved, err := EnsureResolvedInWorkspace(t.Context(), "")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceLocalRuntime, "global-token", false)
		assert.Equal(t, address, resolved.Address)
		assert.Empty(t, resolved.UnixSocket)
	})

	t.Run("injected", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "injected-token")
		ctx := context.WithValue(t.Context(), BaseURLKey{}, "https://daemon.example")

		resolved, err := EnsureResolvedInWorkspace(ctx, "")
		require.NoError(t, err)
		assertResolvedCredential(t, resolved, DaemonSourceInjected, "injected-token", false)
	})
}

func TestResolvedDaemonRunningPreservesEndpointMetadata(t *testing.T) {
	tests := []struct {
		name           string
		in             ResolvedDaemon
		want           RunningDaemon
		wantUnixSocket string
	}{
		{
			name: "local tcp",
			in: localRuntimeResolved(liveDaemon{
				BaseURL: "http://127.0.0.1:27123",
				Record:  kitdaemon.RuntimeRecord{Address: "127.0.0.1:27123"},
			}),
			want: RunningDaemon{BaseURL: "http://127.0.0.1:27123", Address: "127.0.0.1:27123", Network: "tcp", Scheme: "http"},
		},
		{
			name: "local unix",
			in: resolvedForRunning(DaemonSourceLocalRuntime, "",
				localRunningDaemon(UnixBase, "unix:///tmp/kata-test.sock")),
			want:           RunningDaemon{BaseURL: UnixBase, Address: "unix:///tmp/kata-test.sock", Network: "unix", Scheme: "http"},
			wantUnixSocket: "/tmp/kata-test.sock",
		},
		{
			name: "configured remote",
			in:   ResolvedDaemon{Source: DaemonSourceServerEnv, BaseURL: "https://daemon.example", Address: "https://daemon.example", Network: "tcp", Scheme: "https"},
			want: RunningDaemon{BaseURL: "https://daemon.example", Address: "https://daemon.example", Network: "tcp", Scheme: "https", ConfiguredRemote: true},
		},
		{
			name: "injected",
			in:   ResolvedDaemon{Source: DaemonSourceInjected, BaseURL: "http://127.0.0.1:27123", Address: "http://127.0.0.1:27123", Network: "tcp", Scheme: "http"},
			want: RunningDaemon{BaseURL: "http://127.0.0.1:27123", Address: "http://127.0.0.1:27123", Network: "tcp", Scheme: "http"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.in.Running())
			assert.Equal(t, tt.wantUnixSocket, tt.in.UnixSocket)
		})
	}
}

func TestResolvedDaemonWithRunningReplacesOnlyRuntimeMetadata(t *testing.T) {
	original := ResolvedDaemon{
		Source: DaemonSourceNamedCatalog, Name: "local-daemon",
		BaseURL: "http://127.0.0.1:27123", Address: "127.0.0.1:27123",
		Network: "tcp", Scheme: "http",
		Token: "target-token", AllowInsecure: true, TrustPrivateNetwork: true,
	}
	running := RunningDaemon{
		BaseURL: UnixBase, Address: "unix:///tmp/fresh-kata.sock",
		Network: "unix", Scheme: "http",
	}

	refreshed := original.WithRunning(running)

	assert.Equal(t, DaemonSourceNamedCatalog, refreshed.Source)
	assert.Equal(t, "local-daemon", refreshed.Name)
	assert.Equal(t, "target-token", refreshed.Token)
	assert.True(t, refreshed.AllowInsecure)
	assert.True(t, refreshed.TrustPrivateNetwork)
	assert.Equal(t, UnixBase, refreshed.BaseURL)
	assert.Equal(t, "unix:///tmp/fresh-kata.sock", refreshed.Address)
	assert.Equal(t, "unix", refreshed.Network)
	assert.Equal(t, "http", refreshed.Scheme)
	assert.Equal(t, "/tmp/fresh-kata.sock", refreshed.UnixSocket)
}

func TestDiscoverResolvedPreservesScanErrors(t *testing.T) {
	t.Run("runtime store", func(t *testing.T) {
		_, ok, err := DiscoverResolved(t.Context(), "relative-data-dir")
		require.Error(t, err)
		assert.False(t, ok)
		assert.Contains(t, err.Error(), "must be absolute")
	})

	t.Run("context cancellation", func(t *testing.T) {
		home := setupKataEnv(t)
		require.NoError(t, writeRuntimeRecord(t, home, "unix://"+filepath.Join(home, "missing.sock")))
		namespace, err := daemon.NewNamespace()
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, ok, err := DiscoverResolved(ctx, namespace.DataDir)
		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, ok)
	})

	t.Run("unreachable local daemon", func(t *testing.T) {
		home := setupKataEnv(t)
		address := "unix://" + filepath.Join(home, "missing.sock")
		require.NoError(t, writeRuntimeRecord(t, home, address))
		namespace, err := daemon.NewNamespace()
		require.NoError(t, err)

		_, ok, err := DiscoverResolved(t.Context(), namespace.DataDir)
		require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
		assert.False(t, ok)
		assert.Contains(t, err.Error(), address)
	})
}

func TestDiscoverResolvedNamedLocalWithoutLiveDaemon(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "local"
local = true
`))

	resolved, err := DiscoverResolvedNamed(t.Context(), "local")
	require.NoError(t, err)
	assert.Empty(t, resolved.BaseURL)
}

func TestDiscoverResolvedNamedPreservesLocalRuntimeMetadata(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "local"
local = true
`))
	baseURL, address := startMockDaemonPing(t, map[string]any{
		"ok": true, "service": "kata", "version": currentVersionForEnsure(),
	})
	require.NoError(t, writeRuntimeRecord(t, home, address))

	resolved, err := DiscoverResolvedNamed(t.Context(), "local")
	require.NoError(t, err)
	assert.Equal(t, localRunningDaemon(baseURL, address), resolved.Running())
}

func TestDiscoverResolvedNamedPreservesResolutionErrors(t *testing.T) {
	home := setupKataEnv(t)
	require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "remote"
url = "http://127.0.0.1:1"
`))

	_, err := DiscoverResolvedNamed(t.Context(), "remote")
	require.ErrorIs(t, err, ErrRemoteUnavailable)
}

func TestCredentialFreeLocatorsDoNotResolveCatalogTokenEnv(t *testing.T) {
	t.Run("active remote", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv("KATA_MISSING_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
active_daemon = "shared"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_MISSING_TOKEN"
`))

		target, err := LocateRunningTargetInWorkspace(t.Context(), "")
		require.NoError(t, err)
		assert.Equal(t, remoteRunningDaemon(srv.URL, true), target)
	})

	t.Run("named remote", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv("KATA_MISSING_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_MISSING_TOKEN"
`))

		target, err := LocateNamedRunningTarget(t.Context(), "shared")
		require.NoError(t, err)
		assert.Equal(t, remoteRunningDaemon(srv.URL, true), target)
	})
}

func TestRemoteResolutionModesShareSelectionWithoutCredentialLeak(t *testing.T) {
	t.Run("server environment wins for both modes", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", srv.URL)
		t.Setenv("KATA_AUTH_TOKEN", "global-token")
		workspace := t.TempDir()
		writeNeutralWorkspaceMarker(t, workspace)
		require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte(`
version = 1

[server]
url = "http://127.0.0.1:1"
`), 0o600))

		baseURL, ok, err := ResolveRemote(t.Context(), workspace)
		require.NoError(t, err)
		require.True(t, ok)
		resolved, resolvedOK, err := ResolveRemoteDaemon(t.Context(), workspace)
		require.NoError(t, err)
		require.True(t, resolvedOK)

		assert.Equal(t, srv.URL, baseURL)
		assert.Equal(t, baseURL, resolved.BaseURL)
		assert.Equal(t, DaemonSourceServerEnv, resolved.Source)
		assert.Equal(t, "global-token", resolved.Token)
	})

	t.Run("credential-free active selection ignores unset token env", func(t *testing.T) {
		srv := startResolvableDaemon(t)
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_SERVER", "")
		t.Setenv("KATA_AUTH_TOKEN", "")
		t.Setenv("KATA_MISSING_TOKEN", "")
		require.NoError(t, writeRawConfig(home, `
active_daemon = "shared"

[[daemon]]
name = "shared"
url = "`+srv.URL+`"
token_env = "KATA_MISSING_TOKEN"
`))

		baseURL, ok, err := ResolveRemote(t.Context(), "")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, srv.URL, baseURL)

		_, _, err = ResolveRemoteDaemon(t.Context(), "")
		require.EqualError(t, err, `daemon "shared": token_env "KATA_MISSING_TOKEN" is unset or empty`)
	})
}

func assertResolvedCredential(
	t *testing.T,
	resolved ResolvedDaemon,
	wantSource DaemonSource,
	wantToken string,
	wantAllowInsecure bool,
) {
	t.Helper()
	assert.Equal(t, wantSource, resolved.Source)
	assert.Equal(t, wantToken, resolved.Token)
	assert.Equal(t, wantAllowInsecure, resolved.AllowInsecure)
	assert.True(t, resolved.TrustPrivateNetwork)
}

func startResolvableDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeNeutralWorkspaceMarker(t *testing.T, workspace string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte(
		"version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n",
	), 0o600))
}

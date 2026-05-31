package tui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		Name:          "shared",
		URL:           "http://100.64.0.5:7777",
		TokenEnv:      "KATA_SHARED_TOKEN",
		AllowInsecure: true,
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
	oldEnsure := ensureRunningForTUI
	oldEnsureLocal := ensureLocalRunningForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		ensureRunningForTUI = oldEnsure
		ensureLocalRunningForTUI = oldEnsureLocal
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	var ensured bool
	ensureRunningForTUI = func(context.Context) (string, error) {
		t.Fatal("explicit local target must not honor remote-aware EnsureRunning")
		return "", nil
	}
	ensureLocalRunningForTUI = func(context.Context) (string, error) {
		ensured = true
		return "http://kata.invalid", nil
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

func TestBootDaemonConnectionWithoutActiveKeepsRemoteAwareEnsureRunningPath(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldEnsure := ensureRunningForTUI
	oldEnsureLocal := ensureLocalRunningForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		ensureRunningForTUI = oldEnsure
		ensureLocalRunningForTUI = oldEnsureLocal
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{}, nil
	}
	var ensured bool
	ensureRunningForTUI = func(context.Context) (string, error) {
		ensured = true
		return "http://kata.invalid", nil
	}
	ensureLocalRunningForTUI = func(context.Context) (string, error) {
		t.Fatal("implicit default boot must keep existing remote-aware EnsureRunning behavior")
		return "", nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := bootDaemonConnection(context.Background(), Options{})

	require.NoError(t, err)
	assert.True(t, ensured, "implicit daemon must use existing EnsureRunning path")
	assert.Equal(t, "http://kata.invalid", conn.endpoint)
	assert.Equal(t, "local", daemonTargetDisplay(conn.target))
	assert.Equal(t, viewEmpty, conn.init.view)
}

func TestBootDaemonConnectionWithoutActiveLabelsImplicitRemoteEndpoint(t *testing.T) {
	oldRead := readDaemonConfigForTUI
	oldEnsure := ensureRunningForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		readDaemonConfigForTUI = oldRead
		ensureRunningForTUI = oldEnsure
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	readDaemonConfigForTUI = func() (*config.DaemonConfig, error) {
		return &config.DaemonConfig{}, nil
	}
	ensureRunningForTUI = func(context.Context) (string, error) {
		return "http://100.64.0.5:7777", nil
	}
	newHTTPClientForTUI = func(_ context.Context, _ string, _ daemonTarget, _ clientOptsKind) (*http.Client, error) {
		return &http.Client{}, nil
	}
	bootResolveScopeForTUI = func(context.Context, *Client, string) (bootInit, error) {
		return bootInit{view: viewEmpty, scope: scope{empty: true}}, nil
	}

	conn, err := bootDaemonConnection(context.Background(), Options{})

	require.NoError(t, err)
	assert.False(t, conn.target.Local)
	assert.Equal(t, "http://100.64.0.5:7777", conn.target.URL)
	assert.Equal(t, "100.64.0.5:7777", daemonTargetDisplay(conn.target))
}

func TestNewHTTPClientForTUILocalFallsBackToGlobalAuth(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	hc, err := newHTTPClientForTUI(t.Context(), srv.URL, daemonTarget{Local: true}, clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestNewHTTPClientForTUIImplicitRemoteFallsBackToGlobalAuth(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	hc, err := newHTTPClientForTUI(t.Context(), srv.URL, implicitDaemonTarget(srv.URL), clientOptsNormal)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := hc.Do(req) //nolint:gosec // test request targets httptest.Server's loopback URL
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "Bearer global-token", gotAuth)
}

func TestConnectDaemonTargetRemoteUsesPerDaemonAuth(t *testing.T) {
	oldNormalize := normalizeRemoteURLForTUI
	oldProbe := probeRemoteForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		normalizeRemoteURLForTUI = oldNormalize
		probeRemoteForTUI = oldProbe
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})

	target := daemonTarget{Name: "shared", URL: "http://daemon.internal:7777", Token: "tok", AllowInsecure: true}
	var gotNormal, gotSSE daemonTarget
	normalizeRemoteURLForTUI = func(v string, allowInsecure bool) (string, error) {
		require.Equal(t, target.URL, v)
		require.True(t, allowInsecure)
		return "http://daemon.internal:7777", nil
	}
	probeRemoteForTUI = func(context.Context, string) bool { return true }
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

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "http://daemon.internal:7777", conn.endpoint)
	assert.Equal(t, target, gotNormal)
	assert.Equal(t, target, gotSSE)
	assert.Equal(t, "shared", conn.target.Name)
}

func TestConnectDaemonTargetRemoteResolvesTokenEnvOnUse(t *testing.T) {
	oldNormalize := normalizeRemoteURLForTUI
	oldProbe := probeRemoteForTUI
	oldNewClient := newHTTPClientForTUI
	oldBootScope := bootResolveScopeForTUI
	t.Cleanup(func() {
		normalizeRemoteURLForTUI = oldNormalize
		probeRemoteForTUI = oldProbe
		newHTTPClientForTUI = oldNewClient
		bootResolveScopeForTUI = oldBootScope
	})
	t.Setenv("KATA_WORK_TOKEN", "secret-from-env")

	target := daemonTarget{Name: "shared", URL: "https://daemon.example", TokenEnv: "KATA_WORK_TOKEN"} //nolint:gosec // env var name, not a credential
	var gotNormal, gotSSE daemonTarget
	normalizeRemoteURLForTUI = func(v string, _ bool) (string, error) {
		return v, nil
	}
	probeRemoteForTUI = func(context.Context, string) bool { return true }
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

	conn, err := connectDaemonTarget(context.Background(), target)

	require.NoError(t, err)
	assert.Equal(t, "secret-from-env", gotNormal.Token)
	assert.Equal(t, "secret-from-env", gotSSE.Token)
	assert.Equal(t, "secret-from-env", conn.target.Token)
}

func TestConnectDaemonTargetRemoteRejectsUnsetTokenEnvOnUse(t *testing.T) {
	t.Setenv("KATA_WORK_TOKEN", "")

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

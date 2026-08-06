package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenWebUILocalLoopbackOpensDirectly(t *testing.T) {
	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		Runtime: DiscoveredWebRuntime{
			Origin:       "http://127.0.0.1:27123",
			Capabilities: []string{"loopback", "sse"},
		},
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:27123/kata?view=today", opened.PublicURL)
}

func TestResolveWebUIHostTargetUsesLocalGatewayDespiteRemoteOverride(t *testing.T) {
	t.Setenv("KATA_SERVER", "https://daemon.example")
	ctx := context.WithValue(t.Context(), BaseURLKey{}, "http://127.0.0.1:27123")

	baseURL, configuredRemote, allowInsecure, err := resolveWebUIHostTarget(ctx, PrepareWebUIOptions{})

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:27123", baseURL)
	assert.False(t, configuredRemote)
	assert.False(t, allowInsecure)
}

func TestOpenWebUILocalTrustedProxyOpensDirectly(t *testing.T) {
	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		Runtime: DiscoveredWebRuntime{
			Origin: "https://daemon.example", Capabilities: []string{"proxy", "sse"},
		},
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example/kata?view=today", opened.PublicURL)
}

func TestOpenWebUIRemoteLoginRejectsPathPrefix(t *testing.T) {
	opened := false
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL:          "https://daemon.example/kata",
		ConfiguredRemote: true,
	}, "/kata?scope=01HZNQ7VFPK1XGD8R5MABCD4EX&status=open", func(_ context.Context, _ WebUILaunch) error {
		opened = true
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path prefix")
	assert.False(t, opened)
}

func TestOpenWebUIRemoteReadonlyOpensWithoutLogin(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/ui/snapshot", r.URL.Path)
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"contract_version": "1",
			"cursor":           1,
			"capabilities": map[string]any{
				"writable": false, "updates": "poll", "actor_policy": "request",
			},
			"origin": server.URL, "origin_stable": true,
			"catalog": []any{}, "collection": []any{}, "collection_links": []any{},
		}))
	}))
	t.Cleanup(server.Close)

	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: server.URL, ConfiguredRemote: true, AnonymousClient: server.Client(),
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, server.URL+"/kata?view=today", opened.PublicURL)
}

func TestOpenWebUIRemoteReadonlyProbeRejectsRedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	t.Cleanup(target.Close)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	opened := false
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: source.URL, ConfiguredRemote: true, AnonymousClient: source.Client(),
	}, "/kata", func(_ context.Context, _ WebUILaunch) error {
		opened = true
		return nil
	})

	require.Error(t, err)
	assert.False(t, redirected.Load())
	assert.False(t, opened)
}

func TestOpenWebUIRemoteLoginRejectsUntrustedPlaintext(t *testing.T) {
	opened := false
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: "http://daemon.example", ConfiguredRemote: true,
	}, "/kata", func(_ context.Context, _ WebUILaunch) error {
		opened = true
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext non-loopback")
	assert.False(t, opened)
}

func TestOpenWebUIRemoteAuthenticationOpensCanonicalLoginOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Kata-Web-Origin", "https://daemon.example")
		w.Header().Set("X-Kata-Web-Authentication", "login")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: server.URL, ConfiguredRemote: true, AnonymousClient: server.Client(),
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	target, err := url.Parse(opened.PublicURL)
	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example", target.Scheme+"://"+target.Host)
	fragment, err := url.ParseQuery(target.Fragment)
	require.NoError(t, err)
	assert.Equal(t, "1", fragment.Get("login"))
	assert.Equal(t, "/kata?view=today", fragment.Get("return_path"))
}

func TestOpenWebUIRemoteAuthenticationRequiresCanonicalOrigin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	opened := false
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: server.URL, ConfiguredRemote: true, AnonymousClient: server.Client(),
	}, "/kata?view=today", func(_ context.Context, _ WebUILaunch) error {
		opened = true
		return nil
	})

	require.Error(t, err)
	assert.False(t, opened)
}

func TestOpenWebUIConfiguredLoopbackOpensDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kata-Web-Origin", "http://"+r.Host)
		w.Header().Set("X-Kata-Web-Authentication", "loopback")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: server.URL, ConfiguredRemote: true, AnonymousClient: server.Client(),
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, server.URL+"/kata?view=today", opened.PublicURL)
}

func TestOpenWebUIConfiguredTrustedProxyOpensDirectly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Kata-Web-Origin", "https://daemon.example")
		w.Header().Set("X-Kata-Web-Authentication", "proxy")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		BaseURL: server.URL, ConfiguredRemote: true, AnonymousClient: server.Client(),
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example/kata?view=today", opened.PublicURL)
}

func TestOpenWebUILocalReadonlyOpensDirectly(t *testing.T) {
	var opened WebUILaunch
	err := OpenWebUI(t.Context(), PreparedWebUI{
		Runtime: DiscoveredWebRuntime{
			Origin: "http://127.0.0.1:27123", Capabilities: []string{"readonly", "poll"},
		},
	}, "/kata?view=today", func(_ context.Context, target WebUILaunch) error {
		opened = target
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:27123/kata?view=today", opened.PublicURL)
}

func TestOpenWebUIRefusesUnsafeRuntimeMetadata(t *testing.T) {
	opened := false
	unsafeOrigin := "http://user-a:placeholder@daemon.example" //nolint:gosec // Deliberately unsafe userinfo metadata must be refused.
	err := OpenWebUI(t.Context(), PreparedWebUI{
		Runtime: DiscoveredWebRuntime{
			Origin: unsafeOrigin, Capabilities: []string{"loopback"},
		},
	}, "/kata", func(context.Context, WebUILaunch) error {
		opened = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, opened)
}

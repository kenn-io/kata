package daemon

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestResolveWebEndpointDefaultUsesAssignedPort(t *testing.T) {
	resolved, err := ResolveWebEndpoint(WebEndpointOptions{
		NamespaceID: "abc123def456",
		GOOS:        "linux",
		Daemon:      kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/example.sock"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resolved.Listener.Close() })

	host, port, err := net.SplitHostPort(resolved.Endpoint.Address)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", host)
	assert.NotEqual(t, "0", port)
	assert.Equal(t, "http://"+resolved.Endpoint.Address, resolved.Origin)
	assert.False(t, resolved.OriginStable)
	assert.False(t, resolved.Shared)
}

func TestResolveWebEndpointConfiguredZeroUsesAssignedPort(t *testing.T) {
	resolved, err := ResolveWebEndpoint(WebEndpointOptions{
		NamespaceID: "abc123def456",
		GOOS:        "linux",
		Daemon:      kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/example.sock"},
		Config:      config.WebConfig{Listen: "127.0.0.1:0"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resolved.Listener.Close() })

	_, port, err := net.SplitHostPort(resolved.Endpoint.Address)
	require.NoError(t, err)
	assert.NotEqual(t, "0", port)
	assert.Equal(t, "http://"+resolved.Endpoint.Address, resolved.Origin)
	assert.False(t, resolved.OriginStable)
	assert.False(t, resolved.Shared)
}

func TestResolveWebEndpointConfiguredPortCollisionFails(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })

	_, err = ResolveWebEndpoint(WebEndpointOptions{
		NamespaceID: "abc123def456",
		GOOS:        "linux",
		Daemon:      kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/example.sock"},
		Config:      config.WebConfig{Listen: occupied.Addr().String()},
	})
	require.Error(t, err)
}

func TestResolveWebEndpointNonAddressErrorFails(t *testing.T) {
	want := errors.New("listener failed")
	_, err := resolveWebEndpoint(WebEndpointOptions{
		NamespaceID: "abc123def456",
		GOOS:        "linux",
		Daemon:      kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: "/tmp/example.sock"},
	}, func(_, _ string) (net.Listener, error) {
		return nil, want
	})

	assert.ErrorIs(t, err, want)
}

func TestResolveWebEndpointSharesTCPListener(t *testing.T) {
	resolved, err := ResolveWebEndpoint(WebEndpointOptions{
		NamespaceID: "abc123def456",
		GOOS:        "windows",
		Daemon:      kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:27123"},
		Config:      config.WebConfig{PublicOrigin: "https://daemon.example"},
	})
	require.NoError(t, err)

	assert.True(t, resolved.Shared)
	assert.Nil(t, resolved.Listener)
	assert.Equal(t, "https://daemon.example", resolved.Origin)
	assert.True(t, resolved.OriginStable)
}

func TestWebEndpointAllowsLocalSessionOnlyForKeylessDirectLoopback(t *testing.T) {
	loopback := WebEndpoint{
		Endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:27123"},
		Origin:   "http://127.0.0.1:27123",
	}
	assert.True(t, loopback.AllowsLocalSession(config.AuthConfig{}))
	assert.False(t, loopback.AllowsLocalSession(config.AuthConfig{Token: "configured-token"}))
	assert.False(t, loopback.AllowsLocalSession(config.AuthConfig{RequireTokenIdentity: true}))
	assert.False(t, loopback.AllowsLocalSession(config.AuthConfig{
		Proxy: config.ProxyConfig{TrustedActorHeader: "X-Example-Actor"},
	}))

	proxied := loopback
	proxied.Origin = "https://daemon.example"
	assert.False(t, proxied.AllowsLocalSession(config.AuthConfig{}))

	privateNetwork := loopback
	privateNetwork.Endpoint.Address = "100.64.0.5:27123"
	privateNetwork.Origin = "http://100.64.0.5:27123"
	assert.False(t, privateNetwork.AllowsLocalSession(config.AuthConfig{}))
}

func TestWebEndpointAllowsTrustedProxySessionOnlyOnNamedListener(t *testing.T) {
	endpoint := WebEndpoint{
		Endpoint: kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:27123"},
		Origin:   "https://daemon.example",
	}
	assert.True(t, endpoint.AllowsTrustedProxySession(config.AuthConfig{Proxy: config.ProxyConfig{
		TrustedActorHeader:    "X-Kata-Actor",
		TrustedProxyListeners: []string{"127.0.0.1:27123"},
	}}))
	assert.False(t, endpoint.AllowsTrustedProxySession(config.AuthConfig{Proxy: config.ProxyConfig{
		TrustedActorHeader:    "X-Kata-Actor",
		TrustedProxyListeners: []string{"127.0.0.1:27124"},
	}}))
}

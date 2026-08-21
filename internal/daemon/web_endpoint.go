package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/config"
	kitdaemon "go.kenn.io/kit/daemon"
)

// WebEndpointOptions describes the daemon transport and browser-specific
// settings needed to select the browser listener.
type WebEndpointOptions struct {
	NamespaceID string
	GOOS        string
	Daemon      kitdaemon.Endpoint
	Config      config.WebConfig
}

// WebEndpoint is either a browser-owned listener or the daemon's shared TCP
// listener. Listener is nil when Shared is true.
type WebEndpoint struct {
	Endpoint     kitdaemon.Endpoint
	Listener     net.Listener
	Origin       string
	OriginStable bool
	Shared       bool
}

// AllowsLocalSession reports whether this endpoint uses the direct, owner-local
// loopback trust boundary for ordinary browser UI access.
func (e WebEndpoint) AllowsLocalSession(auth config.AuthConfig) bool {
	if auth.RequireTokenIdentity ||
		strings.TrimSpace(auth.Proxy.TrustedActorHeader) != "" || len(auth.Proxy.TrustedProxyListeners) != 0 {
		return false
	}
	endpointHost, _, err := net.SplitHostPort(e.Endpoint.Address)
	if err != nil || !isLoopbackHost(endpointHost) {
		return false
	}
	origin, err := url.Parse(e.Origin)
	if err != nil {
		return false
	}
	originHost := origin.Hostname()
	return (origin.Scheme == "http" || origin.Scheme == "https") && isLoopbackHost(originHost)
}

// AllowsTrustedProxySession reports whether the browser listener itself is a
// configured trusted-proxy listener and can exchange its attributed principal
// for a tab-scoped browser session.
func (e WebEndpoint) AllowsTrustedProxySession(auth config.AuthConfig) bool {
	if strings.TrimSpace(auth.Proxy.TrustedActorHeader) == "" {
		return false
	}
	for _, listener := range auth.Proxy.TrustedProxyListeners {
		if normalizeListenerEntry(listener) == e.Endpoint.Address {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ResolveWebEndpoint binds the browser's secondary listener when the daemon
// transport cannot be shared. The returned listener belongs to the caller.
func ResolveWebEndpoint(options WebEndpointOptions) (WebEndpoint, error) {
	return resolveWebEndpoint(options, net.Listen)
}

func resolveWebEndpoint(
	options WebEndpointOptions,
	listen func(network, address string) (net.Listener, error),
) (WebEndpoint, error) {
	if options.NamespaceID == "" {
		return WebEndpoint{}, errors.New("web endpoint: namespace identity is required")
	}
	if options.Daemon.Network == kitdaemon.NetworkTCP {
		if _, port, err := net.SplitHostPort(options.Daemon.Address); err != nil || port == "0" {
			return WebEndpoint{}, fmt.Errorf("web endpoint: shared daemon address must be concrete: %q", options.Daemon.Address)
		}
		origin := options.Config.PublicOrigin
		if origin == "" {
			origin = "http://" + options.Daemon.Address
		}
		return WebEndpoint{
			Endpoint:     options.Daemon,
			Origin:       origin,
			OriginStable: true,
			Shared:       true,
		}, nil
	}

	address := options.Config.Listen
	if address == "" {
		address = "127.0.0.1:0"
	}
	_, requestedPort, err := net.SplitHostPort(address)
	if err != nil {
		return WebEndpoint{}, fmt.Errorf("web endpoint: invalid listen address %q: %w", address, err)
	}

	listener, err := listen("tcp", address)
	if err != nil {
		return WebEndpoint{}, fmt.Errorf("web endpoint: listen %q: %w", address, err)
	}
	actual := listener.Addr().String()
	origin := options.Config.PublicOrigin
	if origin == "" {
		origin = "http://" + actual
	}
	stable := requestedPort != "0" || options.Config.PublicOrigin != ""
	return WebEndpoint{
		Endpoint:     kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: actual},
		Listener:     listener,
		Origin:       origin,
		OriginStable: stable,
	}, nil
}

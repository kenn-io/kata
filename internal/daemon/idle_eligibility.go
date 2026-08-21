package daemon

import (
	"net"
	"net/url"
	"strings"

	"go.kenn.io/kata/internal/config"
	kitdaemon "go.kenn.io/kit/daemon"
)

// AutostartIdleShutdownEligible reports whether every effective daemon
// exposure remains owner-local. Auto-start provenance is checked by the caller.
func AutostartIdleShutdownEligible(
	daemonEndpoint kitdaemon.Endpoint,
	webEndpoint WebEndpoint,
	cfg config.DaemonConfig,
) bool {
	if !idleLocalEndpoint(daemonEndpoint) || !idleLocalEndpoint(webEndpoint.Endpoint) {
		return false
	}
	if strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader) != "" ||
		len(cfg.Auth.Proxy.TrustedProxyListeners) != 0 {
		return false
	}
	if webEndpoint.Shared && len(cfg.Web.AllowedHosts) != 0 {
		return false
	}
	origin, err := url.Parse(webEndpoint.Origin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return false
	}
	return isLoopbackHost(origin.Hostname())
}

func idleLocalEndpoint(endpoint kitdaemon.Endpoint) bool {
	if endpoint.Network == kitdaemon.NetworkUnix {
		return true
	}
	if endpoint.Network != kitdaemon.NetworkTCP {
		return false
	}
	host, _, err := net.SplitHostPort(endpoint.Address)
	return err == nil && isLoopbackHost(host)
}

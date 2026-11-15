package client

import (
	"context"
	"net/http"
	"os"
)

// configuredEndpoint preserves a selected socket separately from its synthetic
// HTTP request base. HTTP-only origin validators deliberately remain separate.
func configuredEndpoint(source DaemonSource, name, address string, allowInsecure bool) (ResolvedDaemon, error) {
	if _, unix, err := UnixSocketPath(address); err != nil {
		return ResolvedDaemon{}, err
	} else if unix {
		return resolvedForRunning(source, name, RunningDaemon{
			BaseURL: UnixBase, Address: address, Network: "unix", Scheme: "http", ConfiguredRemote: true,
		}), nil
	}
	base, err := normalizeRemoteURL(address, allowInsecure)
	if err != nil {
		return ResolvedDaemon{}, err
	}
	resolved := resolvedForRunning(source, name, remoteRunningDaemon(base, true))
	resolved.AllowInsecure = allowInsecure
	return resolved, nil
}

func probeConfigured(ctx context.Context, target ResolvedDaemon) bool {
	if target.UnixSocket == "" {
		return probeRemote(ctx, target.BaseURL)
	}
	timeout, _ := ParseHTTPTimeout(os.Getenv(HTTPTimeoutEnvVar), DefaultHTTPTimeout)
	transport := UnixTransport(target.UnixSocket)
	defer transport.CloseIdleConnections()
	return Ping(ctx, &http.Client{Transport: transport, Timeout: timeout}, target.BaseURL)
}

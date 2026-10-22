package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonMCPHandler(ctx context.Context, cfg config.MCPConfig, token string, endpoint kitdaemon.Endpoint, auth config.AuthConfig) (http.Handler, error) {
	endpoint, err := daemonMCPSelfEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	resolved := client.ResolvedDaemon{
		Address: endpoint.Address, Network: endpoint.Network, BaseURL: endpoint.BaseURL(),
		Token: auth.Token, TrustPrivateNetwork: auth.TrustPrivateNetwork, Direct: true,
		AllowInsecure: auth.Token == "",
	}
	if endpoint.Network == "unix" {
		resolved.UnixSocket = endpoint.Address
	}
	httpClient, err := httpClientForResolved(ctx, resolved)
	if err != nil {
		return nil, err
	}
	apiClient, longRunningClient, err := newMCPAPIClients(ctx, resolved, httpClient)
	if err != nil {
		return nil, err
	}
	observe, stopReporting := startMCPAgentUseReporter(ctx, apiClient, resolved)
	server, err := mcpserver.New(mcpserver.Options{
		Client: apiClient, LongRunningClient: longRunningClient,
		Scope: mcpserver.NewAllScope(), Actor: cfg.Actor, Version: version.Version,
		ObserveToolCall: observe,
	})
	if err != nil {
		stopReporting()
		return nil, err
	}
	context.AfterFunc(ctx, stopReporting)
	// ApplyListenerPolicy owns the daemon mount's Host validation.
	return newMCPHTTPHandler(server, token, true), nil
}

func daemonMCPSelfEndpoint(endpoint kitdaemon.Endpoint) (kitdaemon.Endpoint, error) {
	if endpoint.Network == kitdaemon.NetworkUnix {
		return endpoint, nil
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return kitdaemon.Endpoint{}, err
	}
	ip := net.ParseIP(host)
	if host == "" || ip.Equal(net.IPv4zero) {
		host = "127.0.0.1"
	} else if ip.Equal(net.IPv6zero) {
		host = "::1"
	}
	endpoint.Address = net.JoinHostPort(host, port)
	return endpoint, nil
}

func resolveDaemonMCPStartup(ctx context.Context, cfg *config.DaemonConfig, endpoint kitdaemon.Endpoint) (string, error) {
	if !cfg.MCP.Enabled {
		return "", nil
	}
	if cfg.Auth.RequireTokenIdentity {
		return "", errors.New(`[mcp] enabled cannot be combined with [auth] require_token_identity: the daemon MCP endpoint calls REST with the bootstrap token, which cannot perform attributed writes; run "kata mcp serve --http" with a user token instead`)
	}
	self, err := daemonMCPSelfEndpoint(endpoint)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cfg.Auth.Proxy.TrustedActorHeader) != "" {
		listener := self.Address
		if self.Network == kitdaemon.NetworkTCP {
			addr, err := net.ResolveTCPAddr("tcp", listener)
			if err != nil {
				return "", fmt.Errorf("resolve daemon MCP listener: %w", err)
			}
			listener = addr.String()
		}
		if daemon.TrustedProxyListener(listener, cfg.Auth.Proxy.TrustedProxyListeners) {
			return "", errors.New(`[mcp] enabled cannot be combined with [auth.proxy] trusted_actor_header: the daemon MCP endpoint's REST calls carry no proxy actor header; run "kata mcp serve --http" instead`)
		}
	}
	cfg.MCP.Actor, _ = resolveActor(ctx, cfg.MCP.Actor, nil)
	cfg.MCP.Actor = strings.TrimSpace(cfg.MCP.Actor)
	if cfg.MCP.Actor == "" {
		return "", errors.New("[mcp]: kata actor is required")
	}
	token, _, err := config.ResolveSecret("", cfg.MCP.TokenFile, cfg.MCP.TokenEnv)
	if err != nil {
		return "", fmt.Errorf("[mcp]: %w", err)
	}
	if token == "" {
		return "", errors.New("[mcp] enabled requires token_file or token_env")
	}
	return token, nil
}

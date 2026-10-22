package main

import (
	"context"
	"net"
	"net/http"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func newDaemonMCPHandler(ctx context.Context, cfg config.MCPConfig, token string, endpoint kitdaemon.Endpoint, auth config.AuthConfig) (http.Handler, error) {
	resolved := client.ResolvedDaemon{
		Address: endpoint.Address, Network: endpoint.Network,
		Token: auth.Token, TrustPrivateNetwork: auth.TrustPrivateNetwork,
	}
	if endpoint.Network == "unix" {
		resolved.UnixSocket = endpoint.Address
		resolved.BaseURL = client.UnixBase
	} else {
		host, port, err := net.SplitHostPort(endpoint.Address)
		if err != nil {
			return nil, err
		}
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::":
			host = "::1"
		}
		resolved.Address = net.JoinHostPort(host, port)
		resolved.BaseURL = "http://" + resolved.Address
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

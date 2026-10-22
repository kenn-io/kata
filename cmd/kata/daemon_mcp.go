package main

import (
	"context"
	"net"
	"net/http"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
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
	apiClient, err := kataclient.NewWithHTTPClient(resolved.BaseURL, httpClient)
	if err != nil {
		return nil, err
	}
	longRunningHTTPClient, err := longRunningClientForResolved(ctx, resolved)
	if err != nil {
		return nil, err
	}
	longRunningClient, err := kataclient.NewWithHTTPClient(resolved.BaseURL, longRunningHTTPClient)
	if err != nil {
		return nil, err
	}
	actor := cfg.Actor
	if actor == "" {
		actor, _ = resolveActor(ctx, "", nil)
	}
	server, err := mcpserver.New(mcpserver.Options{
		Client: apiClient, LongRunningClient: longRunningClient,
		Scope: mcpserver.NewAllScope(), Actor: actor, Version: version.Version,
	})
	if err != nil {
		return nil, err
	}
	return newMCPHTTPHandler(server, token), nil
}

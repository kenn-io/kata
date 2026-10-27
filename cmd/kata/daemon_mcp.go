package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/version"
	kitdaemon "go.kenn.io/kit/daemon"
)

func daemonMCPUnavailableHandler(err error) http.Handler {
	slog.Error("daemon MCP endpoint disabled", "err", err)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"kata MCP endpoint unavailable; see daemon log"}`))
	})
}

func newDaemonMCPHandler(ctx context.Context, store db.Storage, cfg config.MCPConfig, token string, endpoint kitdaemon.Endpoint, auth config.AuthConfig) (http.Handler, error) {
	scope, err := daemonMCPScope(ctx, store, cfg)
	if err != nil {
		return nil, err
	}
	endpoint, err = daemonMCPSelfEndpoint(endpoint)
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
	// Token admin is sidecar-only: the mount needs require_token_identity off, under which token creation 409s.
	server, err := mcpserver.New(mcpserver.Options{
		Client: apiClient, LongRunningClient: longRunningClient,
		Scope: scope, Actor: cfg.Actor, Version: version.Version,
		StartupSections: cfg.StartupSections,
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

func daemonMCPScope(ctx context.Context, store db.Storage, cfg config.MCPConfig) (*mcpserver.Scope, error) {
	if cfg.AllProjects {
		return mcpserver.NewAllScope(), nil
	}
	catalog, err := store.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("[mcp] projects: %w", err)
	}
	projects := make([]mcpserver.ProjectIdentity, 0, len(catalog))
	for _, project := range catalog {
		projects = append(projects, mcpserver.ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
	}
	scope, err := mcpserver.SelectAllowlistScope(projects, cfg.Projects)
	if err != nil {
		return nil, fmt.Errorf("[mcp] projects: %w", err)
	}
	return scope, nil
}

func daemonMCPSelfEndpoint(endpoint kitdaemon.Endpoint) (kitdaemon.Endpoint, error) {
	if endpoint.Network == kitdaemon.NetworkUnix {
		return endpoint, nil
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return kitdaemon.Endpoint{}, err
	}
	// A "tcp" wildcard listener is dual-stack; IPv4 loopback works even where ::1 is absent.
	if ip := net.ParseIP(host); host == "" || ip.IsUnspecified() {
		host = "127.0.0.1"
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
			return "", errors.New(`[mcp] enabled cannot be combined with [auth.proxy] trusted_actor_header: the daemon MCP endpoint's REST calls carry no proxy actor header; run "kata mcp serve --http" with REST access through a listener outside trusted_proxy_listeners or through the proxy that sets the actor header`)
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
	// An all-project mount carries daemon-wide authority, so it must be chosen explicitly.
	switch {
	case cfg.MCP.AllProjects && len(cfg.MCP.Projects) > 0:
		return "", errors.New("[mcp] projects and all_projects cannot both be set")
	case !cfg.MCP.AllProjects && len(cfg.MCP.Projects) == 0:
		return "", errors.New("[mcp] enabled requires projects or all_projects = true")
	}
	for i, section := range cfg.MCP.StartupSections {
		cfg.MCP.StartupSections[i] = strings.TrimSpace(section)
	}
	return token, nil
}

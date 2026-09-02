package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	mcpserver "go.kenn.io/kata/internal/mcp"
	"go.kenn.io/kata/internal/storageadmin"
	"go.kenn.io/kata/internal/version"
	kataclient "go.kenn.io/kata/pkg/client"
)

func newMCPCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "mcp",
		Short: "serve Kata tools through Model Context Protocol",
	}
	command.AddCommand(newMCPServeCmd())
	return command
}

func newMCPServeCmd() *cobra.Command {
	var allProjects bool
	var projects []string
	var storageRoot string
	var storageTargets []string
	var enableTokenAdmin bool
	var httpAddress string
	var httpTokenEnv string
	var trustPrivateNetwork bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "serve Kata tools over stdio or streamable HTTP",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if strings.TrimSpace(storageRoot) == "" && len(storageTargets) > 0 {
				return errors.New("--storage-target requires --storage-root")
			}
			if allProjects && (len(projects) > 0 || strings.TrimSpace(flags.Workspace) != "" || strings.TrimSpace(flags.Project) != "") {
				return errors.New("--all-projects cannot be combined with --projects, --project, or --workspace")
			}
			if len(projects) > 0 && strings.TrimSpace(flags.Workspace) != "" {
				return errors.New("--projects cannot be combined with --workspace")
			}
			if len(projects) > 0 && strings.TrimSpace(flags.Project) != "" {
				return errors.New("--projects cannot be combined with --project")
			}
			if enableTokenAdmin && !allProjects {
				return errors.New("--enable-token-admin requires --all-projects")
			}
			httpToken, err := resolveMCPHTTPToken(httpAddress, httpTokenEnv, trustPrivateNetwork)
			if err != nil {
				return err
			}
			ctx := command.Context()
			var storage *storageadmin.Admin
			if strings.TrimSpace(storageRoot) != "" {
				if err := requireHostLocalExport(ctx); err != nil {
					return fmt.Errorf("enable MCP storage: %w", err)
				}
				targets, err := parseMCPStorageTargets(storageTargets)
				if err != nil {
					return err
				}
				sourceDSN, err := config.KataDSN(ctx)
				if err != nil {
					return err
				}
				storage, err = storageadmin.New(storageadmin.Config{Root: storageRoot, SourceDSN: sourceDSN, Targets: targets})
				if err != nil {
					return fmt.Errorf("configure MCP storage: %w", err)
				}
				defer func() { _ = storage.Close() }()
			}
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			// Ordinary daemon calls retain the CLI request timeout so a
			// stalled daemon cannot consume every MCP tool-call slot.
			httpClient, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			health, err := requireDaemonAPIVersionHealth(
				ctx, httpClient, baseURL, apiVersionMCPServer, "kata mcp serve",
			)
			if err != nil {
				return err
			}
			closeRetryHeadersSupported, _ := apiVersionAtLeast(
				health.APISchemaVersion, apiVersionCloseRetrySafety,
			)
			stopKeepalive := startMCPIdleKeepalive(ctx, httpClient, baseURL, health)
			defer stopKeepalive()
			actor, _ := resolveActor(ctx, flags.As, nil)
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, httpClient)
			if err != nil {
				return err
			}
			// Sync passes, bounded waits, and SSE streams use their own
			// request contexts and may legitimately outlive the ordinary
			// request budget or delay response headers until completion.
			longRunningHTTPClient, err := longRunningClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			longRunningAPIClient, err := kataclient.NewWithHTTPClient(baseURL, longRunningHTTPClient)
			if err != nil {
				return err
			}
			var scope *mcpserver.Scope
			var projectID int64
			var projectName string
			switch {
			case allProjects:
				scope = mcpserver.NewAllScope()
			case len(projects) > 0:
				scope, err = mcpserver.ResolveAllowlistScope(ctx, apiClient, projects)
			default:
				start, startErr := resolveStartPath(flags.Workspace)
				if startErr != nil {
					return startErr
				}
				projectID, projectName, err = resolveProjectIDAndNameWithClient(
					daemonAPI{ctx: ctx, client: httpClient, baseURL: baseURL}, start)
				if err != nil && strings.TrimSpace(flags.Workspace) == "" && strings.TrimSpace(flags.Project) == "" {
					err = fmt.Errorf("%w (run inside a kata workspace, or pass --project, --workspace, --projects, or --all-projects)", err)
				}
				if err == nil {
					scope, err = mcpserver.ResolveBoundScope(ctx, apiClient, mcpserver.ProjectIdentity{ID: projectID, Name: projectName})
				}
			}
			if err != nil {
				return fmt.Errorf("resolve MCP project scope: %w", err)
			}
			server, err := mcpserver.New(mcpserver.Options{
				Client:                     apiClient,
				LongRunningClient:          longRunningAPIClient,
				Scope:                      scope,
				ProjectID:                  projectID,
				ProjectName:                projectName,
				Actor:                      actor,
				Version:                    version.Version,
				StorageAdmin:               storage,
				EnableTokenAdmin:           enableTokenAdmin,
				CloseRetryHeadersSupported: closeRetryHeadersSupported,
			})
			if err != nil {
				return err
			}
			if strings.TrimSpace(httpAddress) != "" {
				return serveMCPHTTP(ctx, command.ErrOrStderr(), httpAddress, httpToken, server)
			}
			transport := mcpserver.NewStdioTransport(
				asReadCloser(command.InOrStdin()),
				command.OutOrStdout(),
			)
			err = server.Run(ctx, transport)
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, sdkmcp.ErrConnectionClosed) {
				return nil
			}
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			return err
		},
	}
	command.Flags().BoolVar(&allProjects, "all-projects", false, "serve every project visible to the selected daemon")
	command.Flags().StringSliceVar(&projects, "projects", nil, "serve only these project names")
	command.Flags().StringVar(&storageRoot, "storage-root", "", "enable host-local JSONL artifacts under this directory")
	command.Flags().StringArrayVar(&storageTargets, "storage-target", nil, "approved import target alias=path-or-DSN (repeatable)")
	command.Flags().BoolVar(&enableTokenAdmin, "enable-token-admin", false, "enable daemon token administration tools")
	command.Flags().StringVar(&httpAddress, "http", "", "serve streamable HTTP on host:port instead of stdio")
	command.Flags().StringVar(&httpTokenEnv, "http-token-env", "", "require an inbound bearer read from this environment variable")
	command.Flags().BoolVar(&trustPrivateNetwork, "trust-private-network", false, "trust plaintext MCP HTTP on a non-loopback private network")
	return command
}

func startMCPIdleKeepalive(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	health daemonAPIHealth,
) context.CancelFunc {
	if health.IdleShutdown == nil {
		return func() {}
	}
	timeout, err := time.ParseDuration(strings.TrimSpace(health.IdleShutdown.Timeout))
	if err != nil || timeout < config.MinAutostartIdleTimeout {
		return func() {}
	}
	return startMCPIdleKeepaliveLoop(ctx, client, baseURL, timeout/2)
}

func startMCPIdleKeepaliveLoop(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	interval time.Duration,
) context.CancelFunc {
	if interval <= 0 {
		return func() {}
	}
	keepaliveCtx, cancel := context.WithCancel(ctx)
	sendMCPIdleKeepalive(keepaliveCtx, client, baseURL)
	go func() {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-keepaliveCtx.Done():
				return
			case <-timer.C:
				sendMCPIdleKeepalive(keepaliveCtx, client, baseURL)
				timer.Reset(interval)
			}
		}
	}()
	return cancel
}

func sendMCPIdleKeepalive(ctx context.Context, client *http.Client, baseURL string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/ping", nil)
	if err != nil {
		return
	}
	request.Header.Set(daemon.IdleKeepaliveHeader, "1")
	response, err := client.Do(request)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

func parseMCPStorageTargets(values []string) (map[string]string, error) {
	targets := make(map[string]string, len(values))
	for index, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			// Never echo the raw value: a malformed DSN can carry credentials.
			return nil, fmt.Errorf("invalid --storage-target at position %d; expected alias=path-or-DSN", index+1)
		}
		alias, target := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, exists := targets[alias]; exists {
			return nil, fmt.Errorf("duplicate --storage-target alias %q", alias)
		}
		targets[alias] = target
	}
	return targets, nil
}

func asReadCloser(reader io.Reader) io.ReadCloser {
	if readCloser, ok := reader.(io.ReadCloser); ok {
		return readCloser
	}
	return io.NopCloser(reader)
}

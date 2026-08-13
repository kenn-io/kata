package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"go.kenn.io/kata/internal/config"
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
	command := &cobra.Command{
		Use:   "serve",
		Short: "serve Kata tools over stdio",
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
			httpClient, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, httpClient)
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
				projectID, projectName, err = resolveProjectIDAndNameWithClient(ctx, httpClient, baseURL, start)
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
				Client:           apiClient,
				Scope:            scope,
				ProjectID:        projectID,
				ProjectName:      projectName,
				Actor:            actor,
				Version:          version.Version,
				StorageAdmin:     storage,
				EnableTokenAdmin: enableTokenAdmin,
			})
			if err != nil {
				return err
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
	return command
}

func parseMCPStorageTargets(values []string) (map[string]string, error) {
	targets := make(map[string]string, len(values))
	for _, value := range values {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid --storage-target %q; expected alias=path-or-DSN", value)
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

package main

import (
	"context"
	"errors"
	"io"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	mcpserver "go.kenn.io/kata/internal/mcp"
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
	return &cobra.Command{
		Use:   "serve",
		Short: "serve the bound project over stdio",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			start, err := resolveStartPath(flags.Workspace)
			if err != nil {
				return err
			}
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			httpClient, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			projectID, projectName, err := resolveProjectIDAndNameWithClient(ctx, httpClient, baseURL, start)
			if err != nil {
				return err
			}
			actor, _ := resolveActor(ctx, flags.As, nil)
			apiClient, err := kataclient.NewWithHTTPClient(baseURL, httpClient)
			if err != nil {
				return err
			}
			server, err := mcpserver.New(mcpserver.Options{
				Client:      apiClient,
				ProjectID:   projectID,
				ProjectName: projectName,
				Actor:       actor,
				Version:     version.Version,
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
}

func asReadCloser(reader io.Reader) io.ReadCloser {
	if readCloser, ok := reader.(io.ReadCloser); ok {
		return readCloser
	}
	return io.NopCloser(reader)
}

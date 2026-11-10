package mcpserver

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

func (h toolHandlers) cronRuntimes(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronProjectInput) (*sdkmcp.CallToolResult, generated.ListCronRuntimesResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, generated.ListCronRuntimesResponse{}, err
	}
	inventory, err := h.options.Client.ListCronRuntimes(ctx, &generated.ListCronRuntimesRequestOptions{PathParams: &generated.ListCronRuntimesPath{ProjectID: project.ID}})
	if err != nil {
		return nil, generated.ListCronRuntimesResponse{}, err
	}
	return successResult(), *inventory, nil
}

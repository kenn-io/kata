package mcpserver

import (
	"context"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	kataclient "go.kenn.io/kata/pkg/client"
)

type TrackerConnectionsInput struct {
	Project string `json:"project,omitempty"`
}
type TrackerOperationInput struct {
	Project   string                      `json:"project,omitempty"`
	Operation kataclient.TrackerOperation `json:"operation"`
}
type TrackerReceiptInput struct {
	Project string `json:"project,omitempty"`
	UID     string `json:"uid"`
}

func registerTrackerTools(server *sdkmcp.Server, h toolHandlers) {
	addTool(server, "kata.tracker_connections", "Tracker accounts", "Discover named tracker accounts granted to the actual authenticated account. Owner/self-call MCP modes cannot borrow account authority.", toolHints(true, false, true), h.trackerConnections)
	addTool(server, "kata.tracker_operation", "Tracker operation", "Bind an explicit account, disable, run or reconcile a tracker. Retain the operation UID and exact request across retries; pending outcomes must be inspected. Unsupported provider operations fail explicitly.", toolHints(false, true, true), h.trackerOperation)
	addTool(server, "kata.tracker_receipt", "Tracker receipt", "Read your retained tracker operation result after a timeout without reexecuting it.", toolHints(true, false, true), h.trackerReceipt)
}
func (h toolHandlers) trackerConnections(ctx context.Context, _ *sdkmcp.CallToolRequest, in TrackerConnectionsInput) (*sdkmcp.CallToolResult, kataclient.TrackerConnections, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, in.Project, false)
	if err != nil {
		return nil, kataclient.TrackerConnections{}, err
	}
	out, err := h.options.Client.TrackerConnections(ctx, project.ID, kataclient.TrackerMCPClient)
	if err != nil {
		return nil, kataclient.TrackerConnections{}, err
	}
	return successResult(), *out, nil
}
func (h toolHandlers) trackerOperation(ctx context.Context, _ *sdkmcp.CallToolRequest, in TrackerOperationInput) (*sdkmcp.CallToolResult, kataclient.TrackerReceipt, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, in.Project, false)
	if err != nil {
		return nil, kataclient.TrackerReceipt{}, err
	}
	out, err := h.withLongRunningClient().options.Client.TrackerOperation(ctx, project.ID, in.Operation, kataclient.TrackerMCPClient)
	if err != nil {
		return nil, kataclient.TrackerReceipt{}, err
	}
	return successResult(), *out, nil
}
func (h toolHandlers) trackerReceipt(ctx context.Context, _ *sdkmcp.CallToolRequest, in TrackerReceiptInput) (*sdkmcp.CallToolResult, kataclient.TrackerReceipt, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, in.Project, false)
	if err != nil {
		return nil, kataclient.TrackerReceipt{}, err
	}
	out, err := h.options.Client.TrackerOperationReceipt(ctx, project.ID, in.UID, kataclient.TrackerMCPClient)
	if err != nil {
		return nil, kataclient.TrackerReceipt{}, err
	}
	return successResult(), *out, nil
}

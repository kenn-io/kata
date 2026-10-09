package mcpserver

import (
	"context"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/pkg/client/generated"
)

// FederationBridgeConnectInput selects a catalog hub and the project to bridge.
type FederationBridgeConnectInput struct {
	Project         string `json:"project"`
	HubCatalog      string `json:"hub_catalog"`
	HubProject      string `json:"hub_project"`
	Preflight       bool   `json:"preflight,omitempty"`
	ServeDownstream *bool  `json:"serve_downstream,omitempty"`
}

// FederationBridgeInput selects a local project for bridge status.
type FederationBridgeInput struct {
	Project string `json:"project"`
}

// FederationBridgeDisconnectInput selects a local project for bridge disconnection or preflight.
type FederationBridgeDisconnectInput struct {
	Project   string `json:"project"`
	Preflight bool   `json:"preflight,omitempty"`
}

// FederationBridgeConnectOutput returns the selected project’s enrollment result.
type FederationBridgeConnectOutput struct {
	Result generated.FederationBridgeBody `json:"result"`
}

// FederationBridgeStatusOutput returns the selected project’s current bridge status.
type FederationBridgeStatusOutput struct {
	Result generated.FederationBridgeStatusBody `json:"result"`
}

// FederationBridgeDisconnectOutput returns the selected project’s disconnection result.
type FederationBridgeDisconnectOutput struct {
	Result generated.FederationBridgeDisconnectResult `json:"result"`
}

func (h toolHandlers) federationBridgeConnect(ctx context.Context, _ *sdkmcp.CallToolRequest, in FederationBridgeConnectInput) (*sdkmcp.CallToolResult, FederationBridgeConnectOutput, error) {
	if err := h.requireAllProjectsScope("bridge connect"); err != nil {
		return nil, FederationBridgeConnectOutput{}, err
	}
	in.Project = strings.TrimSpace(in.Project)
	if err := config.ValidateProjectName(in.Project); err != nil {
		return nil, FederationBridgeConnectOutput{}, err
	}
	serve := true
	if in.ServeDownstream != nil {
		serve = *in.ServeDownstream
	}
	actor := h.options.Actor
	response, err := h.options.Client.ConnectFederationBridge(ctx, &generated.ConnectFederationBridgeRequestOptions{Body: &generated.ConnectFederationBridgeBody{ProjectName: in.Project, HubCatalog: in.HubCatalog, HubProject: in.HubProject, Actor: &actor, Preflight: &in.Preflight, ServeDownstream: &serve}})
	if err != nil {
		return nil, FederationBridgeConnectOutput{}, err
	}
	return successResult(), FederationBridgeConnectOutput{Result: *response}, nil
}
func (h toolHandlers) federationBridgeStatus(ctx context.Context, _ *sdkmcp.CallToolRequest, in FederationBridgeInput) (*sdkmcp.CallToolResult, FederationBridgeStatusOutput, error) {
	if err := h.requireAllProjectsScope("bridge status"); err != nil {
		return nil, FederationBridgeStatusOutput{}, err
	}
	in.Project = strings.TrimSpace(in.Project)
	if err := config.ValidateProjectName(in.Project); err != nil {
		return nil, FederationBridgeStatusOutput{}, err
	}
	response, err := h.options.Client.GetFederationBridgeStatus(ctx, &generated.GetFederationBridgeStatusRequestOptions{PathParams: &generated.GetFederationBridgeStatusPath{ProjectName: in.Project}})
	if err != nil {
		return nil, FederationBridgeStatusOutput{}, err
	}
	return successResult(), FederationBridgeStatusOutput{Result: *response}, nil
}
func (h toolHandlers) federationBridgeDisconnect(ctx context.Context, _ *sdkmcp.CallToolRequest, in FederationBridgeDisconnectInput) (*sdkmcp.CallToolResult, FederationBridgeDisconnectOutput, error) {
	if err := h.requireAllProjectsScope("bridge disconnect"); err != nil {
		return nil, FederationBridgeDisconnectOutput{}, err
	}
	in.Project = strings.TrimSpace(in.Project)
	if err := config.ValidateProjectName(in.Project); err != nil {
		return nil, FederationBridgeDisconnectOutput{}, err
	}
	response, err := h.options.Client.DisconnectFederationBridge(ctx, &generated.DisconnectFederationBridgeRequestOptions{PathParams: &generated.DisconnectFederationBridgePath{ProjectName: in.Project}, Body: &generated.DisconnectFederationBridgeBody{Preflight: &in.Preflight}})
	if err != nil {
		return nil, FederationBridgeDisconnectOutput{}, err
	}
	return successResult(), FederationBridgeDisconnectOutput{Result: *response}, nil
}

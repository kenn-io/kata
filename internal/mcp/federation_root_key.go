package mcpserver

import (
	"context"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/pkg/client/generated"
)

// FederationRootKeyRepinInput selects a root project to repin or preview.
type FederationRootKeyRepinInput struct {
	Project         string `json:"project"`
	Preflight       bool   `json:"preflight,omitempty"`
	PreviousKeyFile string `json:"previous_key_file,omitempty"`
	// StrandRelays accepts that an unsigned repin leaves active relay
	// enrollments unable to sync; the daemon refuses without it.
	StrandRelays bool `json:"strand_relays,omitempty"`
}

// FederationRootKeyRepinOutput returns the repin result.
type FederationRootKeyRepinOutput struct {
	Result generated.FederationRootKeyRepinResult `json:"result"`
}

func (h toolHandlers) federationRootKeyRepin(ctx context.Context, _ *sdkmcp.CallToolRequest, in FederationRootKeyRepinInput) (*sdkmcp.CallToolResult, FederationRootKeyRepinOutput, error) {
	if err := h.requireAllProjectsScope("root key repin"); err != nil {
		return nil, FederationRootKeyRepinOutput{}, err
	}
	in.Project = strings.TrimSpace(in.Project)
	if err := config.ValidateProjectName(in.Project); err != nil {
		return nil, FederationRootKeyRepinOutput{}, err
	}
	body := &generated.RepinFederationRootKeyBody{Preflight: &in.Preflight}
	if in.PreviousKeyFile != "" {
		body.PreviousKeyFile = &in.PreviousKeyFile
	}
	if in.StrandRelays {
		body.StrandRelays = &in.StrandRelays
	}
	response, err := h.options.Client.RepinFederationRootKey(ctx, &generated.RepinFederationRootKeyRequestOptions{PathParams: &generated.RepinFederationRootKeyPath{ProjectName: in.Project}, Body: body})
	if err != nil {
		return nil, FederationRootKeyRepinOutput{}, err
	}
	return successResult(), FederationRootKeyRepinOutput{Result: *response}, nil
}

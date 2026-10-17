package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

type mcpCapabilitiesOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Enabled   bool   `json:"enabled"`
		Endpoint  string `json:"endpoint,omitempty"`
		Transport string `json:"transport,omitempty"`
	}
}

func registerMCPHandlers(api huma.API, cfg ServerConfig) {
	huma.Register(api, huma.Operation{OperationID: "mcpCapabilities", Method: http.MethodGet, Path: "/api/v1/mcp/capabilities"},
		func(ctx context.Context, _ *struct{}) (*mcpCapabilitiesOutput, error) {
			out := &mcpCapabilitiesOutput{CacheControl: "private, no-store"}
			if cfg.MCP != nil {
				enabled, err := cfg.MCP.Available(ctx)
				if err != nil {
					return nil, internalAPIError(err)
				}
				out.Body.Enabled = enabled
				if enabled {
					out.Body.Endpoint = "/mcp"
					out.Body.Transport = "streamable-http"
				}
			}
			return out, nil
		})
}

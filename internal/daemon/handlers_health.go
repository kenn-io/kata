package daemon

import (
	"context"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/version"
)

// registerHealthHandlers installs /api/v1/ping and /api/v1/health on humaAPI.
// /ping is the cheap liveness probe (no DB touch). /health reads
// meta.schema_version and reports uptime relative to cfg.StartedAt.
func registerHealthHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "ping",
		Method:      "GET",
		Path:        "/api/v1/ping",
	}, func(ctx context.Context, _ *struct{}) (*api.PingResponse, error) {
		_ = ctx
		out := &api.PingResponse{}
		out.Body.OK = true
		out.Body.Service = "kata"
		out.Body.Version = version.Version
		out.Body.PID = os.Getpid()
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "health",
		Method:      "GET",
		Path:        "/api/v1/health",
	}, func(ctx context.Context, _ *struct{}) (*api.HealthResponse, error) {
		schema, err := cfg.DB.SchemaVersion(ctx)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.HealthResponse{}
		out.Body.OK = true
		out.Body.DBPath = cfg.DB.Path()
		out.Body.SchemaVersion = schema
		out.Body.Version = version.Version
		out.Body.StartedAt = cfg.StartedAt
		out.Body.Uptime = time.Since(cfg.StartedAt).Round(time.Second).String()
		return out, nil
	})
}

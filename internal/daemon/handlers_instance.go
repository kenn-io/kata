package daemon

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/version"
)

// registerInstanceHandlers installs /api/v1/instance — the local kata
// installation's stable identifier alongside the daemon's build version and
// the database's schema_version. The instance UID is set by db.Open at first
// init and never changes; this endpoint surfaces it for future federation
// spoke discovery and lets the spoke negotiate wire/schema compatibility
// without a follow-up /health round trip.
func registerInstanceHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "instance",
		Method:      "GET",
		Path:        "/api/v1/instance",
	}, func(ctx context.Context, _ *struct{}) (*api.InstanceResponse, error) {
		uid := cfg.DB.InstanceUID()
		if uid == "" {
			return nil, api.NewError(503, "instance_uid_unset",
				"meta.instance_uid not yet set", "", nil)
		}
		schema, err := cfg.DB.SchemaVersion(ctx)
		if err != nil {
			return nil, api.NewError(500, "schema_version_unavailable",
				err.Error(), "", nil)
		}
		sv := int64(schema)
		out := &api.InstanceResponse{}
		out.Body.InstanceUID = uid
		out.Body.Version = version.Version
		out.Body.SchemaVersion = sv
		return out, nil
	})
}

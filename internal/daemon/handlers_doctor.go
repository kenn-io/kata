package daemon

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/diagnostics"
)

func registerDoctorHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "doctor", Method: "GET", Path: pathDoctor}, func(ctx context.Context, _ *struct{}) (*api.DoctorResponse, error) {
		ownerLocal := ownerLocalTransport(ctx)
		if webSessionAuthenticated(ctx) ||
			((insecureReadonlyRequest(ctx) || (cfg.InsecureReadonly && cfg.Auth.Token == "")) && !ownerLocal) ||
			(unauthenticatedPrivateNetworkRequest(ctx) && !ownerLocal) {
			return nil, api.NewError(403, "doctor_forbidden", "operator diagnostics require local owner or administrator authority", "", nil)
		}
		p, ok := PrincipalFromContext(ctx)
		// A mounted host has already authorized this exact manage operation.
		if !ok || p.Kind != PrincipalHost {
			if err := ensureTokenAdminAllowed(ctx); err != nil {
				return nil, err
			}
		}
		out := &api.DoctorResponse{}
		out.Body.Hooks.Hooks = []diagnostics.Hook{}
		if source, ok := cfg.Hooks.(interface{ Diagnostics() diagnostics.Hooks }); ok {
			out.Body.Hooks = source.Diagnostics()
		}
		return out, nil
	})
}

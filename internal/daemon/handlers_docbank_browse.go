package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func revalidateDocbankBrowseProject(ctx context.Context, cfg ServerConfig, project db.Project) error {
	current, err := activeProjectByID(ctx, cfg.DB, project.ID)
	if err != nil {
		return err
	}
	if current.UID != project.UID {
		return projectAccessDenied()
	}
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		// Authorize afresh even when the host supplied no optional lease callback.
		_, err := state.controller.Authorize(ctx, state.request)
		if errors.Is(err, ErrHostAccessDenied) {
			return projectAccessDenied()
		}
		if err != nil {
			return attachmentError(503, "access_unavailable")
		}
	}
	return nil
}

func revalidateDocbankBrowseService(ctx context.Context, cfg ServerConfig, in api.DocbankServiceRequest, operation string) error {
	// Signing admission can be cached; this decision deliberately bypasses it.
	authorization, err := evaluateFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", federationTransportOperation(operation))
	if err != nil {
		return err
	}
	if authorization.projectUID != in.ProjectUID {
		return projectAccessDenied()
	}
	return nil
}

func registerDocbankBrowseHandlers(h huma.API, cfg ServerConfig) {
	const public = "/api/v1/projects/{project_id}/docbank"
	huma.Register(h, huma.Operation{OperationID: "docbankResolve", Method: http.MethodGet, Path: public + "/resolve"}, func(ctx context.Context, in *api.DocbankResolveRequest) (*api.DocbankResolveResponse, error) {
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		body, err := resolveOriginDocbank(ctx, cfg, project, in.Path, "")
		if err != nil {
			return nil, err
		}
		if err := revalidateDocbankBrowseProject(ctx, cfg, project); err != nil {
			return nil, err
		}
		return &api.DocbankResolveResponse{Body: body}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "docbankChildren", Method: http.MethodGet, Path: public + "/children"}, func(ctx context.Context, in *api.DocbankChildrenRequest) (*api.DocbankChildrenResponse, error) {
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		body, err := listOriginDocbank(ctx, cfg, project, in.DocbankBrowseParams, "")
		if err != nil {
			return nil, err
		}
		if err := revalidateDocbankBrowseProject(ctx, cfg, project); err != nil {
			return nil, err
		}
		return &api.DocbankChildrenResponse{Body: body}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "federatedDocbankResolve", Method: http.MethodGet, Path: federatedDocbankPath + "/resolve"}, func(ctx context.Context, in *api.DocbankServiceResolveRequest) (*api.DocbankResolveResponse, error) {
		ctx, project, _, err := authorizeDocbankService(ctx, cfg, in.DocbankServiceRequest, "pull", "federatedDocbankResolve")
		if err != nil {
			return nil, err
		}
		body, err := resolveOriginDocbank(ctx, cfg, project, in.Path, in.Origin)
		if err != nil {
			return nil, err
		}
		if err := revalidateDocbankBrowseService(ctx, cfg, in.DocbankServiceRequest, "federatedDocbankResolve"); err != nil {
			return nil, err
		}
		return &api.DocbankResolveResponse{Body: body}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "federatedDocbankChildren", Method: http.MethodGet, Path: federatedDocbankPath + "/children"}, func(ctx context.Context, in *api.DocbankServiceChildrenRequest) (*api.DocbankChildrenResponse, error) {
		ctx, project, _, err := authorizeDocbankService(ctx, cfg, in.DocbankServiceRequest, "pull", "federatedDocbankChildren")
		if err != nil {
			return nil, err
		}
		body, err := listOriginDocbank(ctx, cfg, project, in.DocbankBrowseParams, in.Origin)
		if err != nil {
			return nil, err
		}
		if err := revalidateDocbankBrowseService(ctx, cfg, in.DocbankServiceRequest, "federatedDocbankChildren"); err != nil {
			return nil, err
		}
		return &api.DocbankChildrenResponse{Body: body}, nil
	})
}

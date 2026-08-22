package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerAuthorRewriteHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "rewriteAuthorIdentity",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/actions/rewrite-author",
	}, func(ctx context.Context, in *api.RewriteAuthorIdentityRequest) (*api.RewriteAuthorIdentityResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Body.From) == "" {
			return nil, api.NewError(http.StatusBadRequest, "validation", "--from is required", "", nil)
		}
		if strings.TrimSpace(in.Body.To) == "" {
			return nil, api.NewError(http.StatusBadRequest, "validation", "--to is required", "", nil)
		}
		result, err := cfg.DB.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{
			ProjectID: in.ProjectID,
			Actor:     actor,
			From:      in.Body.From,
			To:        in.Body.To,
		})
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		}
		if _, ok := errors.AsType[*db.ProjectFederatedError](err); ok {
			return nil, api.NewError(http.StatusConflict, "project_federated", err.Error(), "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		if result.Event != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: result.Event, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(*result.Event)
		}
		return &api.RewriteAuthorIdentityResponse{Body: result}, nil
	})
}

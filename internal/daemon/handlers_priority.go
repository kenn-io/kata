package daemon

import (
	"context"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerPriorityHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "setIssuePriority",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/priority",
	}, func(ctx context.Context, in *api.PriorityRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if err := validatePriorityRange(in.Body.Priority); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UpdatePriority(ctx, issue.ID, in.Body.Priority, actor)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if changed && evt != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: evt, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(*evt)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})
}

// validatePriorityRange enforces the 0..4 range at the API layer so callers
// see a 400 validation error rather than the database CHECK's 500. Nil
// priority (clear) is always valid.
func validatePriorityRange(priority *int64) error {
	if priority == nil {
		return nil
	}
	if *priority < 0 || *priority > 4 {
		return api.NewError(400, "validation",
			"priority must be between 0 and 4", "0 = highest, 4 = lowest", nil)
	}
	return nil
}

// parsePriorityQuery parses a query-string priority filter. Empty string
// means no filter; otherwise must parse as 0..4. Errors are 400-shaped so
// the wire user sees them as validation failures.
func parsePriorityQuery(raw, field string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, api.NewError(400, "validation",
			field+" must be an integer 0..4", "", nil)
	}
	if n < 0 || n > 4 {
		return nil, api.NewError(400, "validation",
			field+" must be between 0 and 4", "0 = highest, 4 = lowest", nil)
	}
	return &n, nil
}

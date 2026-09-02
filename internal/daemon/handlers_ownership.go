package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerOwnershipHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "assignIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/assign",
	}, func(ctx context.Context, in *api.AssignRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		owner := strings.TrimSpace(in.Body.Owner)
		if owner == "" {
			return nil, api.NewError(400, "validation", "owner must be non-empty", "", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, &owner, actor)
		if err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "unassignIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/unassign",
	}, func(ctx context.Context, in *api.UnassignRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		var expectedOwner *string
		if in.Body.ExpectedOwner != nil {
			expected := strings.TrimSpace(*in.Body.ExpectedOwner)
			if expected == "" {
				return nil, api.NewError(400, "validation",
					"expected_owner must be non-empty when provided", "omit expected_owner for an unconditional unassign", nil)
			}
			expectedOwner = &expected
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UnassignOwner(ctx, issue.ID, actor, expectedOwner)
		if err != nil {
			if errors.Is(err, db.ErrOwnerMismatch) {
				var currentOwner any
				if updated.Owner != nil {
					currentOwner = *updated.Owner
				}
				return nil, api.NewError(409, "owner_mismatch",
					"issue owner does not match expected owner",
					"refresh issue state before retrying",
					map[string]any{"expected_owner": *expectedOwner, "current_owner": currentOwner})
			}
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if changed && evt != nil {
			cfg.Publish().Event(in.ProjectID, *evt)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updated
		out.Body.Event = evt
		out.Body.Changed = changed
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "claimIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/claim",
	}, func(ctx context.Context, in *api.ClaimRequest) (*api.ClaimResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if in.Body.Force && in.Body.IfUnowned {
			return nil, api.NewError(400, "validation",
				"force and if_unowned are mutually exclusive", "choose one claim mode", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}

		var result db.ClaimResult
		err = cfg.DB.RetryTransient(ctx, func() error {
			var err error
			if in.Body.IfUnowned {
				result, err = cfg.DB.ClaimOwnerIfUnowned(ctx, issue.ID, actor)
			} else {
				result, err = cfg.DB.ClaimOwner(ctx, issue.ID, actor, in.Body.Force)
			}
			return err
		})
		if errors.Is(err, db.ErrAlreadyClaimed) {
			currentOwner := "unknown"
			if result.CurrentOwner != nil {
				currentOwner = *result.CurrentOwner
			}
			hint := "use --force to reassign"
			if in.Body.IfUnowned {
				hint = "choose another issue, or omit if_unowned only for a deliberate retry"
			}
			return nil, api.NewError(409, "already_claimed",
				fmt.Sprintf("issue is already claimed by %s", currentOwner),
				hint,
				map[string]any{"current_owner": currentOwner})
		}
		if err != nil {
			return nil, internalAPIError(err)
		}

		if result.Changed && result.Event != nil {
			cfg.Publish().Event(in.ProjectID, *result.Event)
		}

		out := &api.ClaimResponse{}
		out.Body.Issue = result.Issue
		out.Body.Event = result.Event
		out.Body.Changed = result.Changed
		out.Body.PreviousOwner = result.PreviousOwner
		return out, nil
	})
}

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
		actor := strings.TrimSpace(in.Body.Actor)
		if err := validateActor(actor); err != nil {
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
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, &owner, actor)
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

	huma.Register(humaAPI, huma.Operation{
		OperationID: "unassignIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/unassign",
	}, func(ctx context.Context, in *api.UnassignRequest) (*api.MutationResponse, error) {
		actor := strings.TrimSpace(in.Body.Actor)
		if err := validateActor(actor); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, nil, actor)
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

	huma.Register(humaAPI, huma.Operation{
		OperationID: "claimIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/claim",
	}, func(ctx context.Context, in *api.ClaimRequest) (*api.ClaimResponse, error) {
		actor := strings.TrimSpace(in.Body.Actor)
		if err := validateActor(actor); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}

		var result db.ClaimResult
		err = db.RetryLockContention(ctx, func() error {
			var err error
			result, err = cfg.DB.ClaimOwner(ctx, issue.ID, actor, in.Body.Force)
			return err
		})
		if errors.Is(err, db.ErrAlreadyClaimed) {
			currentOwner := "unknown"
			if result.CurrentOwner != nil {
				currentOwner = *result.CurrentOwner
			}
			return nil, api.NewError(409, "already_claimed",
				fmt.Sprintf("issue is already claimed by %s", currentOwner),
				"use --force to reassign",
				map[string]any{"current_owner": currentOwner})
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		if result.Changed && result.Event != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: result.Event, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(*result.Event)
		}

		out := &api.ClaimResponse{}
		out.Body.Issue = result.Issue
		out.Body.Event = result.Event
		out.Body.Changed = result.Changed
		out.Body.PreviousOwner = result.PreviousOwner
		return out, nil
	})
}

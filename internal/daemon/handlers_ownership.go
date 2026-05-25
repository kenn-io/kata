package daemon

import (
	"context"
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
		if err := validateActor(in.Body.Actor); err != nil {
			return nil, err
		}
		if strings.TrimSpace(in.Body.Owner) == "" {
			return nil, api.NewError(400, "validation", "owner must be non-empty", "", nil)
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		owner := in.Body.Owner
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, &owner, in.Body.Actor)
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
		if err := validateActor(in.Body.Actor); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.UpdateOwner(ctx, issue.ID, nil, in.Body.Actor)
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
		if err := validateActor(in.Body.Actor); err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}

		// Atomically claim the issue (checks ownership and updates in one transaction)
		result, err := cfg.DB.ClaimOwner(ctx, issue.ID, in.Body.Actor, in.Body.Force)
		if err == db.ErrAlreadyClaimed {
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

package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerDestructiveHandlers installs /actions/delete, /actions/restore, and
// /actions/purge. Delete and purge gate on the X-Kata-Confirm header per spec
// §4.4 / §3.5. Restore is reversible and idempotent so it ships unguarded.
func registerDestructiveHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "deleteIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/delete",
	}, func(ctx context.Context, in *api.DestructiveActionRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Idempotent re-delete must resolve a soft-deleted row.
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedYes)
		if err != nil {
			return nil, err
		}
		project, perr := cfg.DB.ProjectByID(ctx, issue.ProjectID)
		if perr != nil {
			return nil, api.NewError(500, "internal", perr.Error(), "", nil)
		}
		if err := validateConfirm(in.Confirm, "DELETE", project.Name, issue.ShortID); err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.SoftDeleteIssue(ctx, issue.ID, actor)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
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
		OperationID: "restoreIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/restore",
	}, func(ctx context.Context, in *api.RestoreRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Restore must resolve a soft-deleted row by short_id (§6).
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedYes)
		if err != nil {
			return nil, err
		}
		updated, evt, changed, err := cfg.DB.RestoreIssue(ctx, issue.ID, actor)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
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
		OperationID: "purgeIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/purge",
	}, func(ctx context.Context, in *api.DestructiveActionRequest) (*api.PurgeResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		// Purge resolves soft-deleted rows so an operator can still confirm
		// the metadata before the irreversible removal.
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedYes)
		if err != nil {
			return nil, err
		}
		project, perr := cfg.DB.ProjectByID(ctx, issue.ProjectID)
		if perr != nil {
			return nil, api.NewError(500, "internal", perr.Error(), "", nil)
		}
		if err := validateConfirm(in.Confirm, "PURGE", project.Name, issue.ShortID); err != nil {
			return nil, err
		}
		var reasonPtr *string
		if in.Body.Reason != "" {
			r := in.Body.Reason
			reasonPtr = &r
		}
		pl, err := cfg.DB.PurgeIssue(ctx, issue.ID, actor, reasonPtr)
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if pl.PurgeResetAfterEventID != nil {
			cfg.Broadcaster.Broadcast(StreamMsg{
				Kind:      "reset",
				ResetID:   *pl.PurgeResetAfterEventID,
				ProjectID: in.ProjectID,
			})
		}
		out := &api.PurgeResponse{}
		out.Body.PurgeLog = pl
		return out, nil
	})
}

// validateConfirm checks an X-Kata-Confirm header against the verb-specific
// expected value ("DELETE <project>#<short_id>" or
// "PURGE <project>#<short_id>"). Missing header → confirm_required; wrong
// value → confirm_mismatch.
func validateConfirm(got, verb, projectName, shortID string) error {
	expected := fmt.Sprintf("%s %s#%s", verb, projectName, shortID)
	if got == "" {
		return api.NewError(412, "confirm_required",
			"this action requires X-Kata-Confirm",
			"set the header to "+expected, nil)
	}
	if got != expected {
		return api.NewError(412, "confirm_mismatch",
			"X-Kata-Confirm header value does not match",
			"expected "+expected, nil)
	}
	return nil
}

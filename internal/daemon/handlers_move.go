package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerMoveHandlers installs POST /api/v1/projects/{project_id}/issues/{ref}/actions/move.
func registerMoveHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "moveIssue",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/actions/move",
	}, moveIssueHandler(cfg))
}

// activeProjectByUID resolves a target project by its UID and refuses
// archived rows. Returns the api.NewError envelope so the caller can
// `return nil, err`.
func activeProjectByUID(ctx context.Context, store db.Storage, uid string) (db.Project, error) {
	p, err := store.ProjectByUID(ctx, uid)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return db.Project{}, api.NewError(404, "project_not_found", "project not found", "", nil)
		}
		return db.Project{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if p.DeletedAt != nil {
		return db.Project{}, api.NewError(404, "project_not_found", "project not found", "", nil)
	}
	return p, nil
}

func moveIssueHandler(cfg ServerConfig) func(context.Context, *api.MoveIssueRequest) (*api.MoveIssueResponse, error) {
	return func(ctx context.Context, in *api.MoveIssueRequest) (*api.MoveIssueResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if in.Body.ToProjectUID == "" {
			return nil, api.NewError(400, "validation", "to_project_uid must be non-empty", "", nil)
		}
		rev, err := parseIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}

		iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		tgt, err := activeProjectByUID(ctx, cfg.DB, in.Body.ToProjectUID)
		if err != nil {
			return nil, err
		}
		if tgt.ID == in.ProjectID {
			return nil, api.NewError(400, "same_project",
				"to_project_uid resolves to the issue's current project", "", nil)
		}

		res, err := cfg.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID:       iss.ID,
			FromProjectID: in.ProjectID,
			ToProjectID:   tgt.ID,
			IfMatchRev:    rev,
			Actor:         actor,
		})
		var conflict *db.RevisionConflictError
		if errors.As(err, &conflict) {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("issue revision is %d", conflict.CurrentRevision), "", nil)
		}
		var cpErr *db.CrossProjectLinksError
		if errors.As(err, &cpErr) {
			return nil, api.NewError(409, "cross_project_links",
				cpErr.Error(), "remove the cross-project links before moving",
				map[string]any{"blockers": cpErr.Blockers})
		}
		var rpErr *db.RecurrencePinnedError
		if errors.As(err, &rpErr) {
			return nil, api.NewError(409, "recurrence_pinned",
				rpErr.Error(), "unpin the issue from its recurrence before moving", nil)
		}
		if apiErr := federationReadOnlyError(err); apiErr != nil {
			return nil, apiErr
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		out := &api.MoveIssueResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Issue = res.Issue
		out.Body.EventID = res.EventID
		out.Body.NewShortID = res.NewShortID
		out.Body.Changed = true
		return out, nil
	}
}

package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// registerMetadataHandlers installs the metadata patch routes.
func registerMetadataHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "patchIssueMetadata",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/metadata",
	}, patchIssueMetadataHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "patchProjectMetadata",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/metadata",
	}, patchProjectMetadataHandler(cfg))
}

func patchIssueMetadataHandler(cfg ServerConfig) func(context.Context, *api.PatchIssueMetadataRequest) (*api.PatchIssueMetadataResponse, error) {
	return func(ctx context.Context, in *api.PatchIssueMetadataRequest) (*api.PatchIssueMetadataResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		rev, err := parseIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID:    iss.ID,
			IfMatchRev: rev,
			Actor:      actor,
			Patch:      in.Body.Patch,
		})
		var conflict *db.RevisionConflictError
		if errors.As(err, &conflict) {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("issue revision is %d", conflict.CurrentRevision), "", nil)
		}
		if errors.Is(err, metadata.ErrInvalidValue) {
			return nil, api.NewError(400, "invalid_metadata_value", err.Error(), "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		out := &api.PatchIssueMetadataResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Issue = res.Issue
		out.Body.Changed = res.Changed
		if res.Changed {
			ev := res.Event
			out.Body.Event = &ev
		}
		return out, nil
	}
}

func patchProjectMetadataHandler(cfg ServerConfig) func(context.Context, *api.PatchProjectMetadataRequest) (*api.PatchProjectMetadataResponse, error) {
	return func(ctx context.Context, in *api.PatchProjectMetadataRequest) (*api.PatchProjectMetadataResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		rev, err := parseIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		res, err := cfg.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
			ProjectID:  in.ProjectID,
			IfMatchRev: rev,
			Actor:      actor,
			Patch:      in.Body.Patch,
		})
		var conflict *db.RevisionConflictError
		if errors.As(err, &conflict) {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("project revision is %d", conflict.CurrentRevision), "", nil)
		}
		if errors.Is(err, metadata.ErrInvalidValue) {
			return nil, api.NewError(400, "invalid_metadata_value", err.Error(), "", nil)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.PatchProjectMetadataResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Project = res.Project
		out.Body.Changed = res.Changed
		if res.Changed {
			ev := res.Event
			out.Body.Event = &ev
		}
		return out, nil
	}
}

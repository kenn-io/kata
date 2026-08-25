package daemon

import (
	"context"
	"encoding/json"
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

// parseOptionalIfMatchRevision parses the metadata patch endpoints' OPTIONAL
// If-Match header. An absent header returns nil: the patch is unconditional
// last-write-wins, the intended default for convention keys (a stop hook and
// an agent writing work.attention concurrently must never see a spurious
// 412). A present header must still be a well-formed "rev-N" ETag — unlike
// move/recurrences, absence here is a choice, not an error.
func parseOptionalIfMatchRevision(raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	rev, err := parseIfMatchRevision(raw)
	if err != nil {
		return nil, err
	}
	return &rev, nil
}

func patchIssueMetadataHandler(cfg ServerConfig) func(context.Context, *api.PatchIssueMetadataRequest) (*api.PatchIssueMetadataResponse, error) {
	return func(ctx context.Context, in *api.PatchIssueMetadataRequest) (*api.PatchIssueMetadataResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		rev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, iss, actor); err != nil {
			return nil, err
		}
		res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID:    iss.ID,
			IfMatchRev: rev,
			Actor:      actor,
			Patch:      in.Body.Patch,
		})
		if conflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("issue revision is %d", conflict.CurrentRevision), "", nil)
		}
		if errors.Is(err, metadata.ErrInvalidValue) {
			return nil, api.NewError(400, "invalid_metadata_value", err.Error(), "", nil)
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}

		out := &api.PatchIssueMetadataResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Issue = res.Issue
		out.Body.Changed = res.Changed
		if res.Changed {
			ev := res.Event
			out.Body.Event = &ev
			// Wake SSE followers (kata events --tail) and hook consumers on
			// the persisted issue.metadata_updated event. Only broadcast when
			// the patch actually changed something — a no-op patch persists no
			// event and must not spuriously wake subscribers.
			cfg.Publish().Event(in.ProjectID, ev)
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
		rev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		designateInbox := inboxDesignationPatch(in.Body.Patch)
		if designateInbox && len(in.Body.Patch) != 1 {
			return nil, api.NewError(400, "invalid_inbox_designation",
				"Inbox designation cannot be combined with other metadata changes", "", nil)
		}
		ctx, err = authorizeHostProjectScope(ctx, []int64{in.ProjectID}, nil, designateInbox)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		if designateInbox {
			res, err := cfg.DB.DesignateInboxProject(ctx, db.DesignateInboxProjectIn{
				ProjectID: in.ProjectID, IfMatchRev: rev, Actor: actor,
			})
			if conflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
				return nil, api.NewError(412, "revision_conflict",
					fmt.Sprintf("project revision is %d", conflict.CurrentRevision), "", nil)
			}
			if errors.Is(err, db.ErrFederatedReadOnly) {
				return nil, federationReadOnlyError(err)
			}
			if err != nil {
				return nil, internalAPIError(err)
			}
			out := &api.PatchProjectMetadataResponse{}
			out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
			out.Body.Project = res.Project
			out.Body.Changed = res.Changed
			for i := range res.Events {
				event := res.Events[i]
				if event.ProjectID == in.ProjectID {
					out.Body.Event = &event
				}
				cfg.Publish().Event(event.ProjectID, event)
			}
			return out, nil
		}
		res, err := cfg.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
			ProjectID:  in.ProjectID,
			IfMatchRev: rev,
			Actor:      actor,
			Patch:      in.Body.Patch,
		})
		if conflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("project revision is %d", conflict.CurrentRevision), "", nil)
		}
		if errors.Is(err, metadata.ErrInvalidValue) {
			return nil, api.NewError(400, "invalid_metadata_value", err.Error(), "", nil)
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.PatchProjectMetadataResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Project = res.Project
		out.Body.Changed = res.Changed
		if res.Changed {
			ev := res.Event
			out.Body.Event = &ev
			// Wake SSE followers and hook consumers on the persisted
			// project.metadata_updated event. No broadcast on a no-op patch.
			cfg.Publish().Event(in.ProjectID, ev)
		}
		return out, nil
	}
}

func inboxDesignationPatch(patch map[string]json.RawMessage) bool {
	role, ok := patch["role"]
	if !ok {
		return false
	}
	var value string
	return json.Unmarshal(role, &value) == nil && value == "inbox"
}

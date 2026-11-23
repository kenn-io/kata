package daemon

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"maps"

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
		return db.Project{}, internalAPIError(err)
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
		ctx, err = authorizeHostProjectScope(ctx, nil, []string{in.Body.ToProjectUID}, false)
		if err != nil {
			return nil, err
		}
		var rev int64
		if !in.Body.DryRun || in.IfMatch != "" {
			rev, err = parseIfMatchRevision(in.IfMatch)
			if err != nil {
				return nil, err
			}
		}

		iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if in.Body.DryRun && in.IfMatch == "" {
			rev = iss.Revision
		}
		tgt, err := activeProjectByUID(ctx, cfg.DB, in.Body.ToProjectUID)
		if err != nil {
			return nil, err
		}
		if tgt.ID == iss.ProjectID {
			return nil, api.NewError(400, "same_project",
				"to_project_uid resolves to the issue's current project", "", nil)
		}

		move := db.MoveIssueProjectIn{
			IssueID:       iss.ID,
			FromProjectID: iss.ProjectID,
			ToProjectID:   tgt.ID,
			IfMatchRev:    rev,
			Actor:         actor,
			DryRun:        true,
			Single:        in.Body.Single,
			AuthorizeProjects: func(uids []string) error {
				_, err := authorizeHostProjectScope(ctx, nil, uids, false)
				return err
			},
		}
		res, err := cfg.DB.MoveIssueProject(ctx, move)
		if err == nil {
			moveSnapshotActors := make(map[int64]string)
			for _, issue := range res.Issues {
				var refresh claimRefreshResult
				if err := requireFederatedIssueClaimWithResult(ctx, cfg, issue, actor, !in.Body.DryRun, &refresh); err != nil {
					return nil, err
				}
				binding, err := cfg.DB.FederationBindingByProject(ctx, issue.ProjectID)
				if errors.Is(err, db.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, internalAPIError(err)
				}
				if !binding.Enabled || binding.Role != db.FederationRoleSpoke || !binding.PushEnabled {
					continue
				}
				retained, snapshot, pending, err := retainedIssueState(ctx, cfg.DB, binding, issue.UID)
				if err != nil {
					return nil, internalAPIError(err)
				}
				if retained && in.Body.DryRun {
					if err := refreshSpokeClaimStatusForGate(ctx, cfg, binding, issue, &refresh, false); err != nil {
						return nil, err
					}
				}
				if err := retainedMoveError(retained, snapshot, pending, refresh); err != nil {
					return nil, err
				}
				if retained {
					moveSnapshotActors[issue.ID] = binding.Actor
				}
			}
			if !in.Body.DryRun {
				if len(moveSnapshotActors) > 0 {
					move.PrepareSnapshot = func(issue db.Issue, payload string) (string, error) {
						actor, ok := moveSnapshotActors[issue.ID]
						if !ok {
							return payload, nil
						}
						canonical, err := db.CanonicalizeFederationSnapshotAuthors(db.RemoteEvent{
							Type: "issue.snapshot", Payload: jsontext.Value(payload),
						}, actor)
						if err != nil {
							return "", err
						}
						return string(canonical.Payload), nil
					}
				}
				move.DryRun = false
				move.ExpectedIssueIDs = make([]int64, len(res.Issues))
				for i, issue := range res.Issues {
					move.ExpectedIssueIDs[i] = issue.ID
				}
				res, err = cfg.DB.MoveIssueProject(ctx, move)
			}
		}
		if errors.Is(err, db.ErrIssueMoveTreeChanged) {
			return nil, api.NewError(409, "issue_move_tree_changed", "issue subtree changed during claim validation", "retry the move to validate the current subtree", nil)
		}
		if err != nil {
			return nil, moveIssueAPIError(err)
		}
		cfg.Publish().EventsByProject(res.Events)
		out := &api.MoveIssueResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Issue = res.Issue
		out.Body.Issues = res.Issues
		out.Body.EventID = res.EventID
		out.Body.Changed = !in.Body.DryRun
		if !in.Body.DryRun {
			out.Body.NewShortID = &res.NewShortID
		}
		return out, nil
	}
}

// moveIssueAPIError maps a refused move. A descendant's refusal keeps its rule's
// error code and names that descendant so the caller can resolve it.
func moveIssueAPIError(err error) error {
	apiErr := moveIssueRuleAPIError(err)
	descendant, ok := errors.AsType[*db.IssueMoveDescendantError](err)
	if !ok || apiErr.Status >= 500 {
		return apiErr
	}
	ref := descendant.Descendant.ProjectName + "#" + descendant.Descendant.ShortID
	data := maps.Clone(apiErr.Data)
	if data == nil {
		data = map[string]any{}
	}
	data["descendant"] = descendant.Descendant
	return api.NewError(apiErr.Status, apiErr.Code, "descendant "+ref+": "+apiErr.Message, apiErr.Hint, data)
}

func moveIssueRuleAPIError(err error) *api.APIError {
	if errors.Is(err, db.ErrIssueMoveDescendantArchived) {
		return api.NewError(409, "issue_move_descendant_archived",
			"issue is in an archived project", "restore its project or remove its parent link before moving the subtree", nil)
	}
	if conflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
		return api.NewError(412, "revision_conflict",
			fmt.Sprintf("issue revision is %d", conflict.CurrentRevision), "", nil)
	}
	if rpErr, ok := errors.AsType[*db.RecurrencePinnedError](err); ok {
		return api.NewError(409, "recurrence_pinned",
			rpErr.Error(), "unpin the issue from its recurrence before moving", nil)
	}
	if parentErr, ok := errors.AsType[*db.NativeRecurringParentPinnedError](err); ok {
		return api.NewError(409, "recurring_parent_pinned",
			parentErr.Error(), "the job and parent must stay in the same project", nil)
	}
	if collision, ok := errors.AsType[*db.ProjectMergeImportMappingCollisionError](err); ok {
		return api.NewError(409, "issue_move_import_mapping_collision",
			"issue has import mappings that already exist in the target project",
			"resolve import mapping collisions before moving", map[string]any{"mappings": collision.Mappings})
	}
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(404, "issue_not_found", "issue not found", "", nil)
	}
	mapped := externalRootConflictError(err)
	if mapped == nil {
		mapped = federationReadOnlyError(err)
	}
	if mapped == nil {
		mapped = internalAPIError(err)
	}
	if apiErr, ok := errors.AsType[*api.APIError](mapped); ok {
		return apiErr
	}
	return api.NewError(500, "internal", "internal error", "", nil)
}

// retainedMoveError refuses a retained task whose hub copy cannot gain a recovery snapshot.
// Waiting on the hub, or on a pending snapshot, stays retryable.
func retainedMoveError(retained, snapshot, pending bool, refresh claimRefreshResult) error {
	switch {
	case !retained || snapshot && !pending && refresh.known && !refresh.missing:
		return nil
	case !snapshot && !pending && refresh.known && !refresh.missing:
		return retainedMoveNoSnapshotError()
	}
	return api.NewError(503, "federation_offline", "the retained task's recovery snapshot has not reached the hub", "retry after federation sync completes", nil)
}

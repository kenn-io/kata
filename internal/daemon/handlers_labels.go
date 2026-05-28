package daemon

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerLabelsHandlers installs POST/DELETE /labels and GET /labels (counts).
// AddLabelAndEvent and RemoveLabelAndEvent wrap the label mutation, the matching
// issue.labeled / issue.unlabeled event, and the issues.updated_at touch in one
// TX so there's no window where the row mutation lands without its event.
func registerLabelsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "addLabel",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/labels",
	}, addLabelHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "removeLabel",
		Method:      "DELETE",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/labels/{label}",
	}, removeLabelHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listLabels",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/labels",
	}, listLabelsHandler(cfg))
}

func addLabelHandler(cfg ServerConfig) func(context.Context, *api.AddLabelRequest) (*api.AddLabelResponse, error) {
	return func(ctx context.Context, in *api.AddLabelRequest) (*api.AddLabelResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}

		ev := db.LabelEventParams{
			EventType: "issue.labeled",
			Label:     in.Body.Label,
			Actor:     actor,
		}
		row, evt, err := cfg.DB.AddLabelAndEvent(ctx, issue.ID, ev)
		switch {
		case errors.Is(err, db.ErrLabelExists):
			// No-op: re-fetch existing row to populate the response.
			existing, lerr := cfg.DB.LabelByEndpoints(ctx, issue.ID, in.Body.Label)
			if lerr != nil {
				return nil, api.NewError(500, "internal", lerr.Error(), "", nil)
			}
			out := &api.AddLabelResponse{}
			out.Body.Issue = issue
			out.Body.Label = existing
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		case errors.Is(err, db.ErrLabelInvalid):
			return nil, api.NewError(400, "validation",
				"label must match charset [a-z0-9._:-] and length 1..64", "", nil)
		case err != nil:
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		updatedIssue, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.AddLabelResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Label = row
		out.Body.Event = &evt
		out.Body.Changed = true
		return out, nil
	}
}

func removeLabelHandler(cfg ServerConfig) func(context.Context, *api.RemoveLabelRequest) (*api.MutationResponse, error) {
	return func(ctx context.Context, in *api.RemoveLabelRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}

		ev := db.LabelEventParams{
			EventType: "issue.unlabeled",
			Label:     in.Label,
			Actor:     actor,
		}
		evt, err := cfg.DB.RemoveLabelAndEvent(ctx, issue.ID, ev)
		if errors.Is(err, db.ErrNotFound) {
			// Idempotent: the label was never attached → no-op envelope.
			out := &api.MutationResponse{}
			out.Body.Issue = issue
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
		cfg.Hooks.Enqueue(evt)
		updatedIssue, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Event = &evt
		out.Body.Changed = true
		return out, nil
	}
}

func listLabelsHandler(cfg ServerConfig) func(context.Context, *api.LabelsListRequest) (*api.LabelsListResponse, error) {
	return func(ctx context.Context, in *api.LabelsListRequest) (*api.LabelsListResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		counts, err := cfg.DB.LabelCounts(ctx, in.ProjectID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.LabelsListResponse{}
		out.Body.Labels = counts
		return out, nil
	}
}

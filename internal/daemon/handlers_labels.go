package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerLabelsHandlers installs POST/PATCH/DELETE /labels and GET /labels
// (counts). AddLabelAndEvent and RemoveLabelAndEvent wrap the label mutation,
// the matching issue.labeled / issue.unlabeled event, and the issues.updated_at
// touch in one TX so there's no window where the row mutation lands without its
// event. PATCH applies a remove list and an add list through
// ChangeLabelsAndEvents in one TX, so a triage swap never leaves the issue
// carrying both labels or neither.
func registerLabelsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "addLabel",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/labels",
	}, withResolvedProject(cfg, addLabelHandler(cfg)))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "updateLabels",
		Method:      "PATCH",
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/labels",
	}, withResolvedProject(cfg, updateLabelsHandler(cfg)))

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
		ifMatchRev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		if db.IsPriorityLabel(in.Body.Label) {
			return nil, priorityLabelError(ctx, cfg.DB, issue)
		}

		ev := db.LabelEventParams{
			EventType:  "issue.labeled",
			Label:      in.Body.Label,
			Actor:      actor,
			IfMatchRev: ifMatchRev,
		}
		row, evt, err := cfg.DB.AddLabelAndEvent(ctx, issue.ID, ev)
		var revConflict *db.RevisionConflictError
		switch {
		case errors.As(err, &revConflict):
			return nil, issueRevisionConflictError(revConflict.CurrentRevision)
		case errors.Is(err, db.ErrLabelExists):
			// No-op: re-fetch existing row to populate the response.
			existing, lerr := cfg.DB.LabelByEndpoints(ctx, issue.ID, in.Body.Label)
			if lerr != nil {
				return nil, internalAPIError(lerr)
			}
			out := &api.AddLabelResponse{}
			out.Body.Issue = issue
			out.Body.Label = existing
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		case errors.Is(err, db.ErrLabelInvalid):
			return nil, api.NewError(400, "validation", invalidLabelMessage, "", nil)
		case errors.Is(err, db.ErrFederatedReadOnly):
			return nil, federationReadOnlyError(err)
		case err != nil:
			return nil, internalAPIError(err)
		}

		cfg.Publish().Event(evt.ProjectID, evt)
		updatedIssue, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		projected, err := scopedMutationEvent(ctx, cfg.DB, &evt)
		if err != nil {
			return nil, err
		}
		out := &api.AddLabelResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Label = row
		out.Body.Event = projected
		out.Body.Changed = true
		return out, nil
	}
}

const invalidLabelMessage = "label must match charset [a-z0-9._:-] and length 1..64"

func priorityLabelError(ctx context.Context, store db.Storage, issue db.Issue) error {
	project, err := activeProjectByID(ctx, store, issue.ProjectID)
	if err != nil {
		return err
	}
	return api.NewError(400, "validation", "use the priority field: kata edit "+qualifiedID(project.Name, issue.ShortID)+" --priority N or kata.edit with priority", "", nil)
}

func updateLabelsHandler(cfg ServerConfig) func(context.Context, *api.UpdateLabelsRequest) (*api.UpdateLabelsResponse, error) {
	return func(ctx context.Context, in *api.UpdateLabelsRequest) (*api.UpdateLabelsResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		ifMatchRev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		// The stores skip a remove that matches nothing without validating it,
		// so a mistyped remove would leave the issue carrying both labels.
		if slices.ContainsFunc(slices.Concat(in.Body.Add, in.Body.Remove), func(l string) bool { return !db.ValidLabel(l) }) {
			return nil, api.NewError(400, "validation", invalidLabelMessage, "", nil)
		}
		add, remove := in.Body.Add, in.Body.Remove
		if len(add) == 0 && len(remove) == 0 {
			return nil, api.NewError(400, "validation", "add or remove must name at least one label", "", nil)
		}
		removing := make(map[string]struct{}, len(remove))
		for _, l := range remove {
			removing[l] = struct{}{}
		}
		for _, l := range add {
			if _, both := removing[l]; both {
				return nil, api.NewError(400, "validation",
					fmt.Sprintf("label %q is in both add and remove", l), "", nil)
			}
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		if slices.ContainsFunc(add, db.IsPriorityLabel) {
			return nil, priorityLabelError(ctx, cfg.DB, issue)
		}

		labels, events, err := cfg.DB.ChangeLabelsAndEvents(ctx, issue.ID, db.ChangeLabelsParams{
			Add:        add,
			Remove:     remove,
			Actor:      actor,
			IfMatchRev: ifMatchRev,
		})
		var revConflict *db.RevisionConflictError
		switch {
		case errors.As(err, &revConflict):
			return nil, issueRevisionConflictError(revConflict.CurrentRevision)
		case errors.Is(err, db.ErrLabelInvalid):
			return nil, api.NewError(400, "validation", invalidLabelMessage, "", nil)
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
		case errors.Is(err, db.ErrFederatedReadOnly):
			return nil, federationReadOnlyError(err)
		case err != nil:
			return nil, internalAPIError(err)
		}

		// Publish the committed batch before projecting so a projection
		// error cannot leave a committed event unpublished.
		cfg.Publish().EventsByProject(events)
		projectedEvents := make([]db.Event, 0, len(events))
		for i := range events {
			projected, err := scopedMutationEvent(ctx, cfg.DB, &events[i])
			if err != nil {
				return nil, err
			}
			if projected != nil {
				projectedEvents = append(projectedEvents, *projected)
			}
		}
		updatedIssue, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.UpdateLabelsResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Labels = labels
		out.Body.Events = projectedEvents
		out.Body.Changed = len(events) > 0
		return out, nil
	}
}

func removeLabelHandler(cfg ServerConfig) func(context.Context, *api.RemoveLabelRequest) (*api.MutationResponse, error) {
	return func(ctx context.Context, in *api.RemoveLabelRequest) (*api.MutationResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		ifMatchRev, err := parseOptionalIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}

		ev := db.LabelEventParams{
			EventType:  "issue.unlabeled",
			Label:      in.Label,
			Actor:      actor,
			IfMatchRev: ifMatchRev,
		}
		evt, err := cfg.DB.RemoveLabelAndEvent(ctx, issue.ID, ev)
		if conflict, ok := errors.AsType[*db.RevisionConflictError](err); ok {
			return nil, issueRevisionConflictError(conflict.CurrentRevision)
		}
		if errors.Is(err, db.ErrNotFound) {
			// Idempotent: the label was never attached → no-op envelope.
			out := &api.MutationResponse{}
			out.Body.Issue = issue
			out.Body.Event = nil
			out.Body.Changed = false
			return out, nil
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}

		cfg.Publish().Event(evt.ProjectID, evt)
		updatedIssue, err := cfg.DB.IssueByID(ctx, issue.ID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		projected, perr := scopedMutationEvent(ctx, cfg.DB, &evt)
		if perr != nil {
			return nil, perr
		}
		out := &api.MutationResponse{}
		out.Body.Issue = updatedIssue
		out.Body.Event = projected
		out.Body.Changed = true
		return out, nil
	}
}

func listLabelsHandler(cfg ServerConfig) func(context.Context, *api.LabelsListRequest) (*api.LabelsListResponse, error) {
	return func(ctx context.Context, in *api.LabelsListRequest) (*api.LabelsListResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		var counts []db.LabelCount
		_, issueIDs, err := issueScopedMembership(ctx, cfg.DB)
		if err != nil {
			return nil, err
		}
		if issueIDs == nil {
			counts, err = cfg.DB.LabelCounts(ctx, in.ProjectID)
		} else {
			labelsByIssue, labelsErr := cfg.DB.LabelsByIssues(ctx, in.ProjectID, issueIDs)
			err = labelsErr
			byLabel := map[string]int64{}
			for issueID, labels := range labelsByIssue {
				db.RecordIssueScopeTarget(ctx, issueID)
				for _, label := range labels {
					byLabel[label]++
				}
			}
			for label, count := range byLabel {
				counts = append(counts, db.LabelCount{Label: label, Count: count})
			}
			sort.Slice(counts, func(i, j int) bool { return counts[i].Label < counts[j].Label })
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.LabelsListResponse{}
		out.Body.Labels = counts
		return out, nil
	}
}

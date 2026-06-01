package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerRecurrencesHandlers installs the five recurrence CRUD routes.
func registerRecurrencesHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID:   "createRecurrence",
		Method:        "POST",
		Path:          "/api/v1/projects/{project_id}/recurrences",
		DefaultStatus: 201,
	}, createRecurrenceHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listRecurrences",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/recurrences",
	}, listRecurrencesHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "showRecurrence",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/recurrences/{recurrence_uid}",
	}, showRecurrenceHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID: "patchRecurrence",
		Method:      "PATCH",
		Path:        "/api/v1/projects/{project_id}/recurrences/{recurrence_uid}",
	}, patchRecurrenceHandler(cfg))

	huma.Register(humaAPI, huma.Operation{
		OperationID:   "deleteRecurrence",
		Method:        "DELETE",
		Path:          "/api/v1/projects/{project_id}/recurrences/{recurrence_uid}",
		DefaultStatus: 204,
	}, deleteRecurrenceHandler(cfg))
}

// parseIfMatchRevision parses an If-Match header in the form `"rev-N"`
// (quoted ETag, where N is the revision integer) and returns N.
// Returns an api.NewError(400, ...) if the value is missing or malformed.
func parseIfMatchRevision(raw string) (int64, error) {
	if raw == "" {
		return 0, api.NewError(400, "validation", "If-Match header required", "", nil)
	}
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, api.NewError(400, "validation", "If-Match must be a quoted ETag", "", nil)
	}
	inner := raw[1 : len(raw)-1]
	if !strings.HasPrefix(inner, "rev-") {
		return 0, api.NewError(400, "validation", "If-Match must be of the form rev-N", "", nil)
	}
	n, err := strconv.ParseInt(inner[len("rev-"):], 10, 64)
	if err != nil {
		return 0, api.NewError(400, "validation", "If-Match revision is not a valid integer", "", nil)
	}
	return n, nil
}

// activeRecurrenceByUID returns a recurrence by UID only when both the
// recurrence and its parent project are not soft-deleted. All failure modes
// return 404 errors to keep parent-project archival invisible to clients.
func activeRecurrenceByUID(
	ctx context.Context, d db.Storage, projectID int64, recUID string,
) (db.Recurrence, error) {
	rec, err := d.GetRecurrenceByUID(ctx, recUID)
	if err != nil {
		return db.Recurrence{}, api.NewError(404, "not_found",
			fmt.Sprintf("recurrence %q not found", recUID), "", nil)
	}
	if rec.DeletedAt != nil {
		return db.Recurrence{}, api.NewError(404, "not_found",
			fmt.Sprintf("recurrence %q not found", recUID), "", nil)
	}
	if rec.ProjectID != projectID {
		return db.Recurrence{}, api.NewError(404, "not_found",
			fmt.Sprintf("recurrence %q not in project %d", recUID, projectID), "", nil)
	}
	if _, err := activeProjectByID(ctx, d, projectID); err != nil {
		return db.Recurrence{}, err
	}
	return rec, nil
}

func createRecurrenceHandler(cfg ServerConfig) func(context.Context, *api.CreateRecurrenceRequest) (*api.CreateRecurrenceResponse, error) {
	return func(ctx context.Context, in *api.CreateRecurrenceRequest) (*api.CreateRecurrenceResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		rec, err := cfg.DB.CreateRecurrence(ctx, db.CreateRecurrenceIn{
			ProjectID: in.ProjectID,
			Actor:     actor,
			Rule:      in.Body.RRule,
			DTStart:   in.Body.DTStart,
			Timezone:  in.Body.Timezone,
			Template: db.RecurrenceTemplate{
				Title:    in.Body.Template.Title,
				Body:     in.Body.Template.Body,
				Owner:    in.Body.Template.Owner,
				Priority: in.Body.Template.Priority,
				Labels:   in.Body.Template.Labels,
				Metadata: in.Body.Template.Metadata,
			},
		})
		if errors.Is(err, db.ErrLabelInvalid) || errors.Is(err, db.ErrInvalidRecurrence) {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.CreateRecurrenceResponse{}
		out.Body.Recurrence = rec
		return out, nil
	}
}

func listRecurrencesHandler(cfg ServerConfig) func(context.Context, *api.ListRecurrencesRequest) (*api.ListRecurrencesResponse, error) {
	return func(ctx context.Context, in *api.ListRecurrencesRequest) (*api.ListRecurrencesResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		list, err := cfg.DB.ListRecurrencesByProject(ctx, in.ProjectID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		if list == nil {
			list = []db.Recurrence{}
		}
		out := &api.ListRecurrencesResponse{}
		out.Body.Recurrences = list
		return out, nil
	}
}

func showRecurrenceHandler(cfg ServerConfig) func(context.Context, *api.ShowRecurrenceRequest) (*api.ShowRecurrenceResponse, error) {
	return func(ctx context.Context, in *api.ShowRecurrenceRequest) (*api.ShowRecurrenceResponse, error) {
		rec, err := activeRecurrenceByUID(ctx, cfg.DB, in.ProjectID, in.RecurrenceUID)
		if err != nil {
			return nil, err
		}
		out := &api.ShowRecurrenceResponse{}
		out.Body.Recurrence = rec
		return out, nil
	}
}

func patchRecurrenceHandler(cfg ServerConfig) func(context.Context, *api.PatchRecurrenceRequest) (*api.PatchRecurrenceResponse, error) {
	return func(ctx context.Context, in *api.PatchRecurrenceRequest) (*api.PatchRecurrenceResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		rev, err := parseIfMatchRevision(in.IfMatch)
		if err != nil {
			return nil, err
		}
		rec, err := activeRecurrenceByUID(ctx, cfg.DB, in.ProjectID, in.RecurrenceUID)
		if err != nil {
			return nil, err
		}
		update := db.RecurrenceUpdate{
			Rule:     in.Body.RRule,
			DTStart:  in.Body.DTStart,
			Timezone: in.Body.Timezone,
		}
		if in.Body.Template != nil {
			update.TemplateTitle = in.Body.Template.Title
			update.TemplateBody = in.Body.Template.Body
			update.TemplateOwner = in.Body.Template.Owner
			update.TemplatePriority = in.Body.Template.Priority
			update.TemplateLabels = in.Body.Template.Labels
			update.TemplateMetadata = in.Body.Template.Metadata
		}
		res, err := cfg.DB.PatchRecurrence(ctx, db.PatchRecurrenceIn{
			RecurrenceID: rec.ID,
			IfMatchRev:   rev,
			Actor:        actor,
			Update:       update,
		})
		var rce *db.RevisionConflictError
		if errors.As(err, &rce) {
			return nil, api.NewError(412, "revision_conflict",
				fmt.Sprintf("recurrence revision is %d", rce.CurrentRevision), "", nil)
		}
		if errors.Is(err, db.ErrLabelInvalid) || errors.Is(err, db.ErrInvalidRecurrence) {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.PatchRecurrenceResponse{}
		out.ETag = fmt.Sprintf(`"rev-%d"`, res.NewRevision)
		out.Body.Recurrence = res.Recurrence
		out.Body.Changed = res.Changed
		return out, nil
	}
}

func deleteRecurrenceHandler(cfg ServerConfig) func(context.Context, *api.DeleteRecurrenceRequest) (*api.DeleteRecurrenceResponse, error) {
	return func(ctx context.Context, in *api.DeleteRecurrenceRequest) (*api.DeleteRecurrenceResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		rec, err := activeRecurrenceByUID(ctx, cfg.DB, in.ProjectID, in.RecurrenceUID)
		if err != nil {
			return nil, err
		}
		if err := cfg.DB.SoftDeleteRecurrence(ctx, rec.ID, actor); err != nil {
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		return &api.DeleteRecurrenceResponse{}, nil
	}
}

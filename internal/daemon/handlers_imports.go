package daemon

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// registerImportsHandlers installs the generic normalized import endpoint.
func registerImportsHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "importIssues",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/imports",
		// Imports batch many issues (plus comments and links) into a single
		// POST. Huma's default 1 MiB cap (huma.go:1378) rejects realistic
		// migrations from beads/JIRA/etc. — a few hundred issues with
		// comments easily exceeds 1 MiB. 64 MiB covers ~25k issues at
		// typical enrichment ratios while still bounding a runaway client.
		MaxBodyBytes: 64 << 20,
	}, func(ctx context.Context, in *api.ImportRequest) (*api.ImportResponse, error) {
		if err := validateActor(in.Body.Actor); err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}

		items := make([]db.ImportItem, 0, len(in.Body.Items))
		for _, src := range in.Body.Items {
			item := db.ImportItem{
				ExternalID:   src.ExternalID,
				Title:        src.Title,
				Body:         src.Body,
				Author:       src.Author,
				Owner:        src.Owner,
				Priority:     src.Priority,
				Status:       src.Status,
				ClosedReason: src.ClosedReason,
				CreatedAt:    src.CreatedAt,
				UpdatedAt:    src.UpdatedAt,
				ClosedAt:     src.ClosedAt,
				Labels:       src.Labels,
			}
			for _, c := range src.Comments {
				item.Comments = append(item.Comments, db.ImportComment{
					ExternalID: c.ExternalID,
					Author:     c.Author,
					Body:       c.Body,
					CreatedAt:  c.CreatedAt,
				})
			}
			for _, l := range src.Links {
				item.Links = append(item.Links, db.ImportLink{
					Type:             l.Type,
					TargetExternalID: l.TargetExternalID,
				})
			}
			items = append(items, item)
		}

		result, events, err := cfg.DB.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: in.ProjectID,
			Source:    in.Body.Source,
			Actor:     in.Body.Actor,
			Items:     items,
		})
		switch {
		case errors.Is(err, db.ErrImportValidation):
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		case errors.Is(err, db.ErrNotFound):
			return nil, api.NewError(404, "issue_not_found", err.Error(), "", nil)
		case err != nil:
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}

		for i := range events {
			evt := events[i]
			cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: &evt, ProjectID: in.ProjectID})
			cfg.Hooks.Enqueue(evt)
		}

		out := &api.ImportResponse{}
		out.Body = result
		return out, nil
	})
}

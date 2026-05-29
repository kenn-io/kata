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
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
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
		if err := requireFederatedImportClaims(ctx, cfg, in.ProjectID, in.Body.Source, actor, items); err != nil {
			return nil, err
		}

		result, events, err := cfg.DB.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: in.ProjectID,
			Source:    in.Body.Source,
			Actor:     actor,
			Items:     items,
		})
		switch {
		case errors.Is(err, db.ErrImportValidation):
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		case errors.Is(err, db.ErrFederatedReadOnly):
			return nil, federationReadOnlyError(err)
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

func requireFederatedImportClaims(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	source string,
	actor string,
	items []db.ImportItem,
) error {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled {
		return nil
	}
	for _, item := range items {
		issue, needsClaim, err := importItemNeedsFederatedClaim(ctx, cfg.DB, projectID, source, item)
		if err != nil {
			return err
		}
		if needsClaim {
			if err := requireFederatedIssueClaim(ctx, cfg, projectID, issue, actor); err != nil {
				return err
			}
		}
	}
	return nil
}

func importItemNeedsFederatedClaim(
	ctx context.Context,
	store *db.DB,
	projectID int64,
	source string,
	item db.ImportItem,
) (db.Issue, bool, error) {
	mapping, err := store.ImportMappingBySource(ctx, projectID, source, "issue", item.ExternalID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, nil
	}
	if err != nil {
		return db.Issue{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if mapping.IssueID == nil {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", "import issue mapping is missing issue id", "", nil)
	}
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", err.Error(), "", nil)
	}
	if err != nil {
		return db.Issue{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if issue.DeletedAt != nil {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", "mapped import issue is deleted", "", nil)
	}
	if item.UpdatedAt.After(issue.UpdatedAt) {
		return issue, true, nil
	}
	for _, comment := range item.Comments {
		_, err := store.ImportMappingBySource(ctx, projectID, source, "comment", comment.ExternalID)
		if errors.Is(err, db.ErrNotFound) {
			return issue, true, nil
		}
		if err != nil {
			return db.Issue{}, false, api.NewError(500, "internal", err.Error(), "", nil)
		}
	}
	return issue, false, nil
}

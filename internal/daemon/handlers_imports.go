package daemon

import (
	"context"
	"errors"
	"fmt"

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
	states := make(map[string]importClaimItemState, len(items))
	for _, item := range items {
		state, err := importItemFederatedClaimState(ctx, cfg.DB, projectID, source, item)
		if err != nil {
			return err
		}
		states[item.ExternalID] = state
	}
	for _, item := range items {
		state := states[item.ExternalID]
		if state.mapped && state.sourceNewer {
			if err := requireFederatedIssueClaim(ctx, cfg, projectID, state.issue, actor); err != nil {
				return err
			}
		}
		if !state.reconcilesLinks() {
			continue
		}
		peers, err := importItemLinkClaimPeers(ctx, cfg.DB, projectID, source, item, state, states)
		if err != nil {
			return err
		}
		if err := requireFederatedLinkClaims(ctx, cfg, projectID, actor, peers...); err != nil {
			return err
		}
	}
	return nil
}

type importClaimItemState struct {
	issue       db.Issue
	mapped      bool
	sourceNewer bool
}

func (s importClaimItemState) reconcilesLinks() bool {
	return !s.mapped || s.sourceNewer
}

func importItemFederatedClaimState(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	source string,
	item db.ImportItem,
) (importClaimItemState, error) {
	mapping, err := store.ImportMappingBySource(ctx, projectID, source, "issue", item.ExternalID)
	if errors.Is(err, db.ErrNotFound) {
		return importClaimItemState{}, nil
	}
	if err != nil {
		return importClaimItemState{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if mapping.IssueID == nil {
		return importClaimItemState{}, api.NewError(404, "issue_not_found", "import issue mapping is missing issue id", "", nil)
	}
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	if errors.Is(err, db.ErrNotFound) {
		return importClaimItemState{}, api.NewError(404, "issue_not_found", err.Error(), "", nil)
	}
	if err != nil {
		return importClaimItemState{}, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if issue.DeletedAt != nil {
		return importClaimItemState{}, api.NewError(404, "issue_not_found", "mapped import issue is deleted", "", nil)
	}
	return importClaimItemState{
		issue:       issue,
		mapped:      true,
		sourceNewer: item.UpdatedAt.After(issue.UpdatedAt),
	}, nil
}

func importItemLinkClaimPeers(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	source string,
	item db.ImportItem,
	state importClaimItemState,
	states map[string]importClaimItemState,
) ([]db.Issue, error) {
	peers, err := importItemLinkAddClaimPeers(ctx, store, projectID, source, item, state, states)
	if err != nil {
		return nil, err
	}
	removalPeers, err := importItemLinkRemovalClaimPeers(ctx, store, projectID, source, item, state)
	if err != nil {
		return nil, err
	}
	return append(peers, removalPeers...), nil
}

func importItemLinkAddClaimPeers(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	source string,
	item db.ImportItem,
	state importClaimItemState,
	states map[string]importClaimItemState,
) ([]db.Issue, error) {
	if len(item.Links) == 0 {
		return nil, nil
	}
	var out []db.Issue
	for _, importLink := range item.Links {
		target, mapped, err := importLinkClaimTarget(ctx, store, projectID, source, importLink.TargetExternalID, states)
		if err != nil {
			return nil, err
		}
		if !mapped {
			continue
		}
		if state.mapped {
			fromID, toID := state.issue.ID, target.ID
			if importLink.Type == "related" && fromID > toID {
				fromID, toID = toID, fromID
			}
			if _, err := store.LinkByEndpoints(ctx, fromID, toID, importLink.Type); err == nil {
				continue
			} else if !errors.Is(err, db.ErrNotFound) {
				return nil, api.NewError(500, "internal", err.Error(), "", nil)
			}
		}
		out = append(out, target)
	}
	return out, nil
}

func importItemLinkRemovalClaimPeers(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	source string,
	item db.ImportItem,
	state importClaimItemState,
) ([]db.Issue, error) {
	if !state.mapped {
		return nil, nil
	}
	desired := make(map[string]struct{}, len(item.Links))
	for _, importLink := range item.Links {
		desired[importClaimLinkExternalID(item.ExternalID, importLink)] = struct{}{}
	}
	mappings, err := store.ImportMappingsByProjectSource(ctx, projectID, source)
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	var out []db.Issue
	for _, mapping := range mappings {
		if mapping.ObjectType != "link" || mapping.IssueID == nil || *mapping.IssueID != state.issue.ID {
			continue
		}
		if _, keep := desired[mapping.ExternalID]; keep {
			continue
		}
		if mapping.LinkID == nil {
			continue
		}
		link, err := store.LinkByID(ctx, *mapping.LinkID)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		peerID := link.ToIssueID
		if peerID == state.issue.ID {
			peerID = link.FromIssueID
		}
		peer, err := store.IssueByID(ctx, peerID)
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out = append(out, peer)
	}
	return out, nil
}

func importLinkClaimTarget(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	source string,
	externalID string,
	states map[string]importClaimItemState,
) (db.Issue, bool, error) {
	if state, ok := states[externalID]; ok {
		return state.issue, state.mapped, nil
	}
	mapping, err := store.ImportMappingBySource(ctx, projectID, source, "issue", externalID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", fmt.Sprintf("import link target %q not found", externalID), "", nil)
	}
	if err != nil {
		return db.Issue{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if mapping.IssueID == nil {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", fmt.Sprintf("import link target %q is missing issue id", externalID), "", nil)
	}
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	if errors.Is(err, db.ErrNotFound) {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", fmt.Sprintf("import link target %q not found", externalID), "", nil)
	}
	if err != nil {
		return db.Issue{}, false, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if issue.DeletedAt != nil {
		return db.Issue{}, false, api.NewError(404, "issue_not_found", fmt.Sprintf("import link target %q is deleted", externalID), "", nil)
	}
	return issue, true, nil
}

func importClaimLinkExternalID(issueExternalID string, link db.ImportLink) string {
	return issueExternalID + ":" + link.Type + ":" + link.TargetExternalID
}

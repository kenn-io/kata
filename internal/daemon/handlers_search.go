package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
)

// registerSearchHandlers installs GET /api/v1/projects/{id}/search. Returns the
// spec §4.10 envelope: query echo, effective mode, optional degraded fields,
// and ranked results with mode-scoped score + matched_in. The lexical and
// vector legs run concurrently; see hybridSearch.
func registerSearchHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "searchIssues",
		Method:      "GET",
		Path:        "/api/v1/projects/{project_id}/search",
	}, func(ctx context.Context, in *api.SearchRequest) (*api.SearchResponse, error) {
		if strings.TrimSpace(in.Query) == "" {
			return nil, api.NewError(400, "validation",
				"query parameter q must be non-empty", "", nil)
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		mode := in.Mode
		if insecureReadonlyRequest(ctx) && cfg.Embedder != nil {
			switch mode {
			case "hybrid", "semantic":
				return nil, api.NewError(401, "auth_required",
					"semantic search requires authentication; daemon is in --insecure-readonly mode", "", nil)
			case "", "auto":
				mode = "lexical"
			}
		}
		res, err := hybridSearch(ctx, cfg.DB, cfg.Embedder, hybridParams{
			ProjectID: in.ProjectID, Query: in.Query, Limit: limit,
			IncludeDeleted: in.IncludeDeleted, Requested: mode,
		})
		if err != nil {
			var me *modeError
			if errors.As(err, &me) {
				kind := "validation"
				if me.Status() == 503 {
					kind = "unavailable"
				}
				return nil, api.NewError(me.Status(), kind, me.Error(), "", nil)
			}
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.SearchResponse{}
		out.Body.Query = in.Query
		out.Body.Mode = string(res.Mode)
		out.Body.Degraded = res.Degraded
		out.Body.DegradedReason = res.DegradedReason
		out.Body.Results = make([]api.SearchHit, 0, len(res.Hits))
		for _, c := range res.Hits {
			out.Body.Results = append(out.Body.Results, api.SearchHit{
				Issue:     c.Issue,
				Score:     c.Score,
				MatchedIn: c.MatchedIn,
			})
		}
		return out, nil
	})
}

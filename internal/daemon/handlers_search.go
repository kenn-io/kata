package daemon

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
)

type hubSearchError struct {
	err         error
	unreachable bool
}

func (e *hubSearchError) Error() string {
	if e.unreachable {
		return "hub unreachable: " + e.err.Error()
	}
	return "hub search failed: " + e.err.Error()
}
func (e *hubSearchError) Unwrap() error { return e.err }

func wrapHubSearchError(err error) error {
	_, transport := errors.AsType[*url.Error](err)
	return &hubSearchError{err: err, unreachable: transport || errors.Is(err, errClaimHubTransportUnavailable)}
}

// registerSearchHandlers installs GET /api/v1/projects/{id}/search. Returns the
// spec §4.10 envelope: query echo, effective mode, optional degraded fields,
// and ranked results with mode-scoped score + matched_in. The lexical and
// vector legs run concurrently; see hybridSearch.
func registerSearchHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "searchIssues",
		Description: "Explicit semantic/hybrid modes return a validation error (400) when a configured embedding credential source is unusable or the provider rejects authentication or access. Default search returns lexical results with degraded_reason instead. Transient vector failures return unavailable (503) in explicit modes.",
		Errors:      []int{400, 401, 403, 404, 503},
		Responses: map[string]*huma.Response{
			"default": {Description: "Error"},
		},
		Method: "GET",
		Path:   "/api/v1/projects/{project_id}/search",
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
		binding, bindingErr := cfg.DB.FederationBindingByProject(ctx, in.ProjectID)
		if bindingErr != nil && !errors.Is(bindingErr, db.ErrNotFound) {
			return nil, internalAPIError(bindingErr)
		}
		direct := bindingErr == nil && binding.IsDirectSpoke()
		if insecureReadonlyRequest(ctx) && (cfg.Embedder != nil || direct) {
			switch mode {
			case "hybrid", "semantic":
				return nil, api.NewError(401, "auth_required",
					"semantic search requires authentication; daemon is in --insecure-readonly mode", "", nil)
			case "", "auto":
				mode = "lexical"
			}
		}

		if !direct && cfg.EmbeddingUnavailableReason != "" && (mode == "semantic" || mode == "hybrid") {
			return nil, api.NewError(503, "unavailable", cfg.EmbeddingUnavailableReason, "", nil)
		}
		stable := in.Sort == "oldest"
		if (stable && in.Mode != "lexical") || (in.Cursor != "" && !stable) {
			return nil, api.NewError(400, "validation", "cursor pagination requires mode=lexical and sort=oldest", "", nil)
		}
		probeLimit, err := pageProbeLimit(limit)
		if err != nil {
			return nil, err
		}
		var res hybridResult
		hash := ""
		remoteURLs := map[string]string{}
		var hubMode, hubDegradedReason string
		if stable {
			params := db.SearchFTSParams{ProjectID: in.ProjectID, Query: in.Query, Status: in.Status, IncludeDeleted: in.IncludeDeleted, Labels: in.Labels, ExcludeLabels: in.ExcludeLabels, IssueScope: issueScopeFromContext(ctx), StableOrder: true}
			normalized := params
			normalized.Labels = normalizePageStrings(in.Labels, true)
			normalized.ExcludeLabels = normalizePageStrings(in.ExcludeLabels, true)
			hash = pagination.Fingerprint(struct {
				Instance string
				Filters  db.SearchFTSParams
			}{cfg.DB.InstanceUID(), normalized})
			params.After, err = pagination.Decode(in.Cursor, hash)
			if err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
			params.Limit = probeLimit
			res.Hits, err = cfg.DB.SearchFTS(ctx, params)
			res.Mode = modeLexical
		} else {
			params := hybridParams{
				ProjectID: in.ProjectID, Query: in.Query, Limit: limit, ProbeLimit: probeLimit,
				IncludeDeleted: in.IncludeDeleted, Requested: mode,
				Labels: in.Labels, ExcludeLabels: in.ExcludeLabels, Status: in.Status,
				IssueScope:              issueScopeFromContext(ctx),
				EmbeddingProjectAllowed: cfg.EmbeddingProjectAllowed,
			}
			if direct {
				params.HubMode = &hubMode
				params.Hub = func(ctx context.Context, p hybridParams, fetch int) ([]api.FederationSearchHit, bool, error) {
					ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
					defer cancel()
					client, cred, err := claimForwardClient(ctx, cfg, binding)
					if err != nil {
						return nil, false, wrapHubSearchError(err)
					}
					body, err := client.SearchShared(ctx, cred.HubProjectID, p, fetch)
					if err != nil {
						return nil, false, wrapHubSearchError(err)
					}
					hubMode, hubDegradedReason = body.Mode, body.DegradedReason
					for _, hit := range body.Hits {
						remoteURLs[hit.Issue.UID] = hit.WebURL
					}
					return body.Hits, body.Bounded, nil
				}
			}
			res, err = hybridSearch(ctx, cfg.DB, cfg.VectorIndex, cfg.Embedder, params)
		}
		if err != nil {
			if busy := forwardedHubBusy(err); busy != nil {
				return nil, busy
			}
			if me, ok := errors.AsType[*modeError](err); ok {
				kind := "validation"
				if me.Status() == 503 {
					kind = "unavailable"
				}
				if me.code != "" {
					kind = me.code
				}
				return nil, api.NewError(me.Status(), kind, me.Error(), "", nil)
			}
			return nil, internalAPIError(err)
		}
		if !direct {
			markEmbeddingUnavailable(&res, cfg.EmbeddingUnavailableReason, mode)
		}
		if hubDegradedReason != "" {
			res.Degraded, res.DegradedReason = true, "hub search: "+hubDegradedReason
		}
		more := len(res.Hits) > limit
		page := api.PageMetadata{Complete: !more, Truncated: more}
		if !stable && (res.Mode != modeLexical || res.Degraded || len(res.Hits) >= 200) {
			page.Complete = false
			page.Truncated = true
		}
		if more {
			res.Hits = res.Hits[:limit]
			if stable {
				last := res.Hits[len(res.Hits)-1].Issue
				page.NextCursor = pagination.Encode(pagination.Position{CreatedAt: last.CreatedAt, ID: last.ID}, hash)
			}
		}
		out := &api.SearchResponse{EmbeddingsConfigured: strconv.FormatBool(cfg.Embedder != nil || direct)}
		out.Body.PageMetadata = page
		out.Body.Query = in.Query
		out.Body.Mode = string(res.Mode)
		out.Body.Degraded = res.Degraded
		out.Body.DegradedReason = res.DegradedReason
		out.Body.Results = make([]api.SearchHit, 0, len(res.Hits))
		for _, c := range res.Hits {
			webURL := remoteURLs[c.Issue.UID]
			if c.Issue.ID != 0 {
				webURL = issueWebURL(cfg, c.Issue.UID)
			}
			out.Body.Results = append(out.Body.Results, api.SearchHit{
				Issue:     c.Issue,
				WebURL:    webURL,
				Score:     c.Score,
				MatchedIn: c.MatchedIn,
			})
		}
		return out, nil
	})
}

// markEmbeddingUnavailable labels an auto search that ran lexically because
// the embedding index cannot be opened. Explicit modes are rejected earlier.
func markEmbeddingUnavailable(res *hybridResult, reason, mode string) {
	if reason != "" && (mode == "" || mode == "auto") {
		res.Degraded = true
		res.DegradedReason = reason
	}
}

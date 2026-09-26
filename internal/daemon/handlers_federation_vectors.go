package daemon

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"regexp"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	katauid "go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/internal/vector"
)

// maxFederationVectorLookupDocs caps one lookup request, matching the spoke's
// batch size.
const maxFederationVectorLookupDocs = 64

// maxFederationVectorLookupBytes bounds the raw vector bytes in one response.
// Records past the budget are answered "deferred" so the spoke asks again.
// The first ok record is always included, so no document is starved.
const maxFederationVectorLookupBytes = 16 << 20

var contentSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func registerFederationVectorHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "lookupFederationProjectVectors",
		Method:      "POST",
		Path:        "/api/v1/projects/{project_id}/federation/vectors:lookup",
		Summary:     "Look up hub-computed issue vectors",
		Description: "Returns the hub's stored chunk vectors for issues whose content hash matches. The hub never embeds on request; a missing vector is not_ready.",
	}, func(ctx context.Context, in *api.FederationVectorLookupRequest) (*api.FederationVectorLookupResponse, error) {
		var err error
		ctx, _, err = authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull",
			federationTransportOperation("lookupFederationProjectVectors"))
		if err != nil {
			return nil, err
		}
		if in.ProjectID <= 0 {
			return nil, api.NewError(http.StatusBadRequest, "validation", "project_id must be a positive integer", "", nil)
		}
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if err := validateFederationVectorLookup(in.Body); err != nil {
			return nil, err
		}
		gen, err := hubVectorGeneration(ctx, cfg)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.FederationVectorLookupResponse{Body: api.FederationVectorLookupBody{
			Generation: gen,
			Records:    []api.FederationVectorRecord{},
		}}
		if gen == nil || in.Body.Fingerprint != gen.Fingerprint || len(in.Body.Docs) == 0 {
			return out, nil
		}
		uids := make([]string, len(in.Body.Docs))
		for i, doc := range in.Body.Docs {
			uids[i] = doc.IssueUID
		}
		stored, err := cfg.VectorIndex.LookupVectors(ctx, gen.Fingerprint, project.UID, uids)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out.Body.Records = federationVectorRecords(in.Body.Docs, stored, maxFederationVectorLookupBytes)
		return out, nil
	})
}

func validateFederationVectorLookup(body api.FederationVectorLookupRequestBody) error {
	if len(body.Docs) > maxFederationVectorLookupDocs {
		return api.NewError(http.StatusBadRequest, "validation",
			fmt.Sprintf("docs must contain at most %d entries", maxFederationVectorLookupDocs), "", nil)
	}
	seen := make(map[string]struct{}, len(body.Docs))
	for _, doc := range body.Docs {
		if !katauid.Valid(doc.IssueUID) {
			return api.NewError(http.StatusBadRequest, "validation", "issue_uid must be a valid UID", "", nil)
		}
		if !contentSHA256Pattern.MatchString(doc.ContentSHA256) {
			return api.NewError(http.StatusBadRequest, "validation",
				"content_sha256 must be 64 lowercase hex characters", "", nil)
		}
		if _, dup := seen[doc.IssueUID]; dup {
			return api.NewError(http.StatusBadRequest, "validation", "issue_uid must not repeat", "", nil)
		}
		seen[doc.IssueUID] = struct{}{}
	}
	return nil
}

// hubVectorGeneration describes the generation this daemon serves vectors
// from: its configured embedder's generation, whether still building or
// active. Serving the building generation lets spokes follow a hub rebuild
// (for example the one-time re-embed after an upgrade) instead of falling
// back to local embedding while the hub's old generation is still active.
// Nil means this daemon has no embeddings configured.
func hubVectorGeneration(ctx context.Context, cfg ServerConfig) (*api.FederationVectorGeneration, error) {
	if cfg.Embedder == nil || cfg.VectorIndex == nil {
		return nil, nil
	}
	gen := cfg.Embedder.Generation()
	key := gen.Fingerprint()
	state, err := cfg.VectorIndex.GenerationState(ctx, key)
	if err != nil {
		return nil, err
	}
	if state == "" {
		state = "pending"
	}
	return &api.FederationVectorGeneration{
		Fingerprint: key, Model: gen.Model, Dims: gen.Dimensions, Params: maps.Clone(gen.Params), State: state,
	}, nil
}

// federationVectorRecords answers docs in request order from stored vectors.
// A document is ok or skipped only when the hub's content hash equals the
// requested one; otherwise the spoke's text differs from what the hub
// embedded and the record is not_ready.
func federationVectorRecords(docs []api.FederationVectorLookupDoc, stored map[string]vector.StoredVectors, budget int) []api.FederationVectorRecord {
	records := make([]api.FederationVectorRecord, 0, len(docs))
	spent := 0
	for _, doc := range docs {
		rec := api.FederationVectorRecord{IssueUID: doc.IssueUID, Status: api.FederationVectorStatusNotReady}
		sv, ok := stored[doc.IssueUID]
		switch {
		case !ok:
		case sv.ContentSHA256 != doc.ContentSHA256:
			rec.ContentSHA256 = sv.ContentSHA256
		case len(sv.Chunks) == 0:
			rec.ContentSHA256 = sv.ContentSHA256
			rec.Status = api.FederationVectorStatusSkipped
		default:
			size := 0
			for _, c := range sv.Chunks {
				size += 4 * len(c.Vector)
			}
			rec.ContentSHA256 = sv.ContentSHA256
			if spent > 0 && spent+size > budget {
				rec.Status = api.FederationVectorStatusDeferred
				break
			}
			spent += size
			rec.Status = api.FederationVectorStatusOK
			rec.Chunks = make([]api.FederationVectorChunk, len(sv.Chunks))
			for i, c := range sv.Chunks {
				rec.Chunks[i] = api.FederationVectorChunk{Index: c.ChunkIndex, Vector: vector.EncodeVector(c.Vector)}
			}
		}
		records = append(records, rec)
	}
	return records
}

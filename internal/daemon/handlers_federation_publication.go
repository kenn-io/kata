package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationpublication"
)

func registerFederationPublication(h huma.API, cfg ServerConfig) {
	const path = "/api/v1/federation/publications"
	huma.Register(h, huma.Operation{
		OperationID: "publish-federation-project", Method: http.MethodPost, Path: path,
		// Huma rejects count == MaxBodyBytes, so allow one sentinel byte to
		// accept the inclusive request maximum shared with the client.
		MaxBodyBytes: int64(db.MaxFederationPublicationRequestBytes) + 1,
		Errors:       []int{400, 401, 403, 404, 409, 413, 500, 503},
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{
			"application/json": {Schema: h.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[api.PublishFederationProjectBody](), true, "PublishFederationProjectBody")},
		}},
		// The codec validates the original closed object, so unsupported policy
		// fields are refused rather than discarded by typed unmarshalling.
		SkipValidateBody: true,
	}, func(ctx context.Context, in *api.PublishFederationProjectRequest) (*api.PublishFederationProjectResponse, error) {
		if err := ensureAttributedWriteAllowed(ctx); err != nil {
			return nil, err
		}
		principal, ok := PrincipalFromContext(ctx)
		if !ok || principal.Kind != PrincipalDBToken || principal.TokenID <= 0 || principal.Scope != nil {
			return nil, api.NewError(403, "relay_user_credential_required", "publication requires an ordinary unscoped account credential", "", nil)
		}
		ctx, actor, err := cronWriteContext(ctx, principal.Actor)
		if err != nil {
			return nil, err
		}
		store, ok := cfg.DB.(db.FederationPublicationStore)
		if !ok {
			return nil, publicationAPIError(nil)
		}
		var request struct {
			Preflight *bool          `json:"preflight"`
			Envelope  jsontext.Value `json:"envelope"`
		}
		if err := json.Unmarshal(in.RawBody, &request, json.RejectUnknownMembers(true)); err != nil || request.Preflight == nil {
			return nil, publicationAPIError(db.ErrFederationPublicationInvalid)
		}
		if reader, ok := cfg.DB.(interface{ FederationPublicationCapacity() int64 }); ok && int64(len(request.Envelope)) > reader.FederationPublicationCapacity() {
			return nil, publicationAPIError(db.ErrFederationPublicationTooLarge)
		}
		document, counts, err := federationpublication.DecodeWithCounts(request.Envelope)
		if err != nil {
			return nil, publicationAPIError(err)
		}
		if document.TargetInstanceUID != cfg.DB.InstanceUID() {
			return nil, publicationAPIError(db.ErrFederationPublicationConflict)
		}
		out := &api.PublishFederationProjectResponse{Body: api.FederationPublicationBody{
			Status: "ready", ProjectUID: document.ProjectUID, ProjectName: document.ProjectName, Account: actor,
			// Native project creation has no policy row: both stores resolve
			// its visibility to all. Team membership never changes this default.
			Visibility: "all", Counts: api.FederationPublicationCounts{Issues: counts.Issues, Comments: counts.Comments, Events: counts.Events},
		}}
		if *request.Preflight {
			for _, lookup := range []func() (db.Project, error){
				func() (db.Project, error) { return cfg.DB.ProjectByNameIncludingArchived(ctx, document.ProjectName) },
				func() (db.Project, error) { return cfg.DB.ProjectByUID(ctx, document.ProjectUID) },
			} {
				if _, err := lookup(); err == nil {
					return nil, publicationAPIError(db.ErrFederationPublicationConflict)
				} else if !errors.Is(err, db.ErrNotFound) {
					return nil, publicationAPIError(err)
				}
			}
			return out, nil
		}
		result, err := store.PublishFederationProject(ctx, document, actor)
		if err != nil {
			return nil, publicationAPIError(err)
		}
		// The transaction has committed. Publish before any fallible response
		// reads; retained historical events are never re-emitted as new actions.
		if result.CatalogEvent != nil {
			deliverProjectMutation(cfg, result.CatalogEvent)
		}
		if !result.Replayed {
			recordProjectAccessCatalogMutation(ctx, result.Project.UID)
		}
		if err := authorizeProjectTarget(ctx, result.Project.UID); err != nil {
			return nil, err
		}
		policy, err := cfg.DB.ProjectAccessPolicy(ctx, result.Project.UID)
		if err != nil {
			return nil, publicationAPIError(err)
		}
		out.Body.Status, out.Body.ProjectID = "published", result.Project.ID
		out.Body.ProjectUID, out.Body.ProjectName = result.Project.UID, result.Project.Name
		out.Body.Visibility, out.Body.Replayed = policy.Visibility, result.Replayed
		return out, nil
	})
	// RawBody is for exact validation, not a second binary upload format.
	openAPI := h.OpenAPI()
	if openAPI == nil || openAPI.Paths == nil {
		return
	}
	pathItem := openAPI.Paths[path]
	if pathItem == nil || pathItem.Post == nil || pathItem.Post.RequestBody == nil || pathItem.Post.RequestBody.Content == nil {
		return
	}
	delete(pathItem.Post.RequestBody.Content, "application/octet-stream")
}

func publicationAPIError(err error) error {
	if known, ok := errors.AsType[*api.APIError](err); ok {
		return known
	}
	switch {
	case err == nil:
		return api.NewError(409, "federation_publication_unsupported", "hub does not support relay publication", "upgrade the selected hub before publishing", nil)
	case errors.Is(err, db.ErrFederationPublicationConflict):
		return api.NewError(409, "federation_publication_conflict", "publication conflicts with existing state", "", nil)
	case errors.Is(err, db.ErrFederationPublicationInvalid):
		return api.NewError(400, "federation_publication_invalid", "invalid publication envelope or unsupported restriction", "", nil)
	case errors.Is(err, db.ErrFederationPublicationTooLarge):
		return api.NewError(413, "federation_publication_too_large", "publication exceeds relay capacity", "", nil)
	case errors.Is(err, db.ErrTransactionFinalizationFailed):
		return api.NewError(503, "access_unavailable", "transaction finalization is unavailable", "", nil)
	case errors.Is(err, ErrHostAccessDenied):
		return projectAccessDenied()
	default:
		return api.NewError(500, "internal", "publication storage is unavailable", "", nil)
	}
}

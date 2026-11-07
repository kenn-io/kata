package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

// ErrLocalEmbeddingsBusy reports an already active setup job.
var ErrLocalEmbeddingsBusy = errors.New("local embeddings setup is already running")

// LocalEmbeddings provides setup owned by the daemon process.
type LocalEmbeddings interface {
	Status(context.Context) (api.LocalEmbeddingsStatus, error)
	TurnOn(context.Context) error
}

func localEmbeddingsAdministrator(ctx context.Context, cfg ServerConfig) bool {
	return projectOwnerAuthority(ctx) && !cfg.InsecureReadonly && (cfg.WebSessions == nil || cfg.WebSessions.Writable())
}

func registerLocalEmbeddingsHandlers(humaAPI huma.API, cfg ServerConfig) {
	setupStatus := func(ctx context.Context) (api.LocalEmbeddingsStatus, error) {
		if cfg.LocalEmbeddings == nil {
			return api.LocalEmbeddingsStatus{}, api.NewError(404, "not_found", "local embeddings setup is unavailable", "", nil)
		}
		snapshot, err := cfg.LocalEmbeddings.Status(ctx)
		if err != nil {
			return snapshot, internalAPIError(err)
		}
		snapshot.CanChange = snapshot.CanChange && localEmbeddingsAdministrator(ctx, cfg)
		return snapshot, nil
	}
	status := func(ctx context.Context) (*api.LocalEmbeddingsResponse, error) {
		snapshot, err := setupStatus(ctx)
		if err != nil {
			return nil, err
		}
		owner := localEmbeddingsAdministrator(ctx, cfg)
		if !owner && snapshot.Job != nil && snapshot.Job.Error != "" {
			snapshot.Job.Error = "Search by meaning could not be turned on. Ask a daemon administrator for details."
		}
		if cfg.ReconcilerHealth != nil {
			h := cfg.ReconcilerHealth()
			if h.LastSuccessAt == nil {
				snapshot.Indexing = &api.LocalEmbeddingsIndexing{Failed: h.LastError != ""}
				if owner {
					snapshot.Indexing.Done, snapshot.Indexing.Total, snapshot.Indexing.ETASeconds = h.Embedded+h.Skipped, h.Embedded+h.Skipped+h.Backlog, h.ETASeconds
				}
			}
		}
		if snapshot.Rebuild {
			ctx, err = authorizeHostProjectScope(ctx, nil, nil, true)
			if err != nil {
				return nil, err
			}
			count, err := cfg.DB.CountAllIssues(ctx, db.ListAllIssuesParams{IssueScope: issueScopeFromContext(ctx)})
			if err != nil {
				return nil, internalAPIError(err)
			}
			snapshot.Issues = &count
		}
		return &api.LocalEmbeddingsResponse{Body: snapshot}, nil
	}
	huma.Register(humaAPI, huma.Operation{OperationID: "getLocalEmbeddings", Method: http.MethodGet, Path: "/api/v1/embeddings/local", Errors: []int{404}}, func(ctx context.Context, _ *struct{}) (*api.LocalEmbeddingsResponse, error) { return status(ctx) })
	huma.Register(humaAPI, huma.Operation{OperationID: "turnOnLocalEmbeddings", Method: http.MethodPost, Path: "/api/v1/embeddings/local", DefaultStatus: http.StatusAccepted, Errors: []int{403, 404, 409}}, func(ctx context.Context, _ *struct{}) (*api.LocalEmbeddingsResponse, error) {
		snapshot, err := setupStatus(ctx)
		if err != nil {
			return nil, err
		}
		if !snapshot.CanChange {
			return nil, api.NewError(403, "forbidden", "local embeddings setup requires daemon administration", "", nil)
		}
		if err := cfg.LocalEmbeddings.TurnOn(ctx); err != nil {
			if errors.Is(err, ErrLocalEmbeddingsBusy) {
				return nil, api.NewError(409, "conflict", err.Error(), "", nil)
			}
			return nil, internalAPIError(err)
		}
		return status(ctx)
	})
}

package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
)

// ErrLocalEmbeddingsBusy reports an already active setup job.
var ErrLocalEmbeddingsBusy = errors.New("local embeddings setup is already running")

// ErrLocalEmbeddingsConfigured reports a daemon already running with a provider.
var ErrLocalEmbeddingsConfigured = errors.New("search by meaning is already configured")

// LocalEmbeddings provides setup owned by the daemon process.
type LocalEmbeddings interface {
	Status(context.Context) (api.LocalEmbeddingsStatus, error)
	TurnOn(context.Context) error
}

func localEmbeddingsAdministrator(ctx context.Context, insecureReadonly bool, sessions *WebSessionManager) bool {
	return projectOwnerAuthority(ctx) && !insecureReadonly && (sessions == nil || sessions.Writable())
}

// redactLocalEmbeddingsStatus removes the setup detail and indexing counts
// reserved for daemon administrators while keeping the active indexing state.
func redactLocalEmbeddingsStatus(snapshot *api.LocalEmbeddingsStatus) {
	snapshot.CanChange = false
	if snapshot.Job != nil && snapshot.Job.Error != "" {
		snapshot.Job.Error = "Search by meaning could not be turned on. Ask a daemon administrator for details."
	}
	if snapshot.Indexing != nil {
		snapshot.Indexing.Done, snapshot.Indexing.Total, snapshot.Indexing.ETASeconds = 0, 0, nil
	}
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
		return snapshot, nil
	}
	status := func(ctx context.Context) (*api.LocalEmbeddingsResponse, error) {
		snapshot, err := setupStatus(ctx)
		if err != nil {
			return nil, err
		}
		if cfg.ReconcilerHealth != nil {
			h := cfg.ReconcilerHealth()
			if h.LastSuccessAt == nil || h.Backlog > 0 || h.LastError != "" {
				snapshot.Indexing = &api.LocalEmbeddingsIndexing{
					Done: h.Embedded + h.Skipped, Total: h.Embedded + h.Skipped + h.Backlog,
					ETASeconds: h.ETASeconds, Failed: h.LastError != "",
				}
			}
		}
		if !localEmbeddingsAdministrator(ctx, cfg.InsecureReadonly, cfg.WebSessions) {
			redactLocalEmbeddingsStatus(&snapshot)
		}
		return &api.LocalEmbeddingsResponse{Body: snapshot}, nil
	}
	huma.Register(humaAPI, huma.Operation{OperationID: "getLocalEmbeddings", Method: http.MethodGet, Path: "/api/v1/embeddings/local", Errors: []int{404}}, func(ctx context.Context, _ *struct{}) (*api.LocalEmbeddingsResponse, error) { return status(ctx) })
	huma.Register(humaAPI, huma.Operation{OperationID: "turnOnLocalEmbeddings", Method: http.MethodPost, Path: "/api/v1/embeddings/local", DefaultStatus: http.StatusAccepted, Errors: []int{403, 404, 409}}, func(ctx context.Context, _ *struct{}) (*api.LocalEmbeddingsResponse, error) {
		if cfg.LocalEmbeddings == nil {
			return nil, api.NewError(404, "not_found", "local embeddings setup is unavailable", "", nil)
		}
		// Authority is checked apart from setup state, so a busy job reads as a conflict.
		if !localEmbeddingsAdministrator(ctx, cfg.InsecureReadonly, cfg.WebSessions) {
			return nil, api.NewError(403, "forbidden", "local embeddings setup requires daemon administration", "", nil)
		}
		if err := cfg.LocalEmbeddings.TurnOn(ctx); err != nil {
			if errors.Is(err, ErrLocalEmbeddingsBusy) || errors.Is(err, ErrLocalEmbeddingsConfigured) {
				return nil, api.NewError(409, "conflict", err.Error(), "", nil)
			}
			return nil, internalAPIError(err)
		}
		return status(ctx)
	})
}

package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
)

func registerTranscriptServiceHandlers(humaAPI huma.API, cfg ServerConfig) {
	// Freeze an operator-owned map at registration; request handlers do not
	// consult the CLI home or a caller-mutable configuration map.
	policy, configErr := config.NormalizeTranscriptServiceConfig(cfg.TranscriptService)
	cfg.TranscriptService = policy
	huma.Register(humaAPI, huma.Operation{OperationID: "getProjectTranscriptService", Method: http.MethodGet, Path: api.TranscriptServicePath},
		func(ctx context.Context, in *api.TranscriptServiceRequest) (*api.TranscriptServiceResponse, error) {
			if configErr != nil {
				return nil, transcriptUnavailable()
			}
			result, err := discoverTranscriptService(ctx, cfg, in.ProjectID)
			if err != nil {
				return nil, err
			}
			if err := revalidateTranscriptReader(ctx, result.Descriptor.ProjectUID); err != nil {
				return nil, err
			}
			return &api.TranscriptServiceResponse{CacheControl: "private, no-store", Body: result}, nil
		})
	huma.Register(humaAPI, huma.Operation{OperationID: "getFederationTranscriptService", Method: http.MethodGet, Path: api.FederationTranscriptServicePath},
		func(ctx context.Context, in *api.FederationTranscriptServiceRequest) (*api.TranscriptServiceResponse, error) {
			if configErr != nil {
				return nil, transcriptUnavailable()
			}
			operation := federationTransportOperation("getFederationTranscriptService")
			ctx, principal, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", operation)
			if err != nil {
				return nil, err
			}
			if principal.ScopedProjectID != in.ProjectID {
				return nil, projectAccessDenied()
			}
			project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
			if err != nil {
				return nil, err
			}
			pin, err := cfg.DB.RootAuthority(ctx, project.UID)
			if err != nil || project.UID != in.ProjectUID || pin.AuthorityUID != in.AuthorityUID {
				return nil, transcriptUnavailable()
			}
			result, err := discoverTranscriptService(ctx, cfg, in.ProjectID)
			if err != nil {
				return nil, err
			}
			// Deliberately uncached: the signing/authorization context may hold
			// the earlier decision from before network I/O.
			if _, err := evaluateFederationRequestForCapabilities(ctx, cfg, in.Authorization, in.ProjectID, []string{"pull"}, operation); err != nil {
				return nil, err
			}
			return &api.TranscriptServiceResponse{CacheControl: "private, no-store", Body: result}, nil
		})
}

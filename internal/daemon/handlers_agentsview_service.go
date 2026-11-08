package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func registerAgentsViewServiceHandlers(humaAPI huma.API, cfg ServerConfig) {
	// Freeze an operator-owned map at registration; request handlers do not
	// consult the CLI home or a caller-mutable configuration map.
	policy, configErr := config.NormalizeAgentsViewConfig(cfg.AgentsView)
	cfg.AgentsView = policy
	huma.Register(humaAPI, huma.Operation{OperationID: "getProjectTranscriptService", Method: http.MethodGet, Path: api.AgentsViewServicePath},
		func(ctx context.Context, in *api.AgentsViewServiceRequest) (*api.AgentsViewServiceResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, agentsViewRequestTimeout)
			defer cancel()
			if configErr != nil {
				return nil, agentsViewUnavailable()
			}
			result, err := discoverAgentsViewService(ctx, cfg, in.ProjectID)
			if err != nil {
				return nil, err
			}
			if err := revalidateAgentsViewReader(ctx, result.Descriptor.ProjectUID); err != nil {
				return nil, err
			}
			return &api.AgentsViewServiceResponse{CacheControl: "private, no-store", Body: result}, nil
		})
	huma.Register(humaAPI, huma.Operation{OperationID: "getFederationTranscriptService", Method: http.MethodGet, Path: api.FederationAgentsViewServicePath},
		func(ctx context.Context, in *api.FederationAgentsViewServiceRequest) (*api.AgentsViewServiceResponse, error) {
			ctx, cancel := context.WithTimeout(ctx, agentsViewRequestTimeout)
			defer cancel()
			if configErr != nil {
				return nil, agentsViewUnavailable()
			}
			operation := federationTransportOperation("getFederationTranscriptService")
			ctx, principal, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", operation)
			if err != nil {
				return nil, err
			}
			if !agentsViewRelayGenerationMatches(principal, in.ProjectID, in.ProtocolVersion, in.BindingUID, in.PeerInstanceUID, in.ResetEpoch) {
				return nil, agentsViewUnavailable()
			}
			project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
			if err != nil {
				return nil, err
			}
			pin, err := cfg.DB.RootAuthority(ctx, project.UID)
			if err != nil || project.UID != in.ProjectUID || pin.AuthorityUID != in.AuthorityUID {
				return nil, agentsViewUnavailable()
			}
			result, err := discoverAgentsViewService(ctx, cfg, in.ProjectID)
			if err != nil {
				return nil, err
			}
			// Deliberately uncached: the signing/authorization context may hold
			// the earlier decision from before network I/O.
			current, err := evaluateFederationRequestForCapabilities(ctx, cfg, in.Authorization, in.ProjectID, []string{"pull"}, operation)
			if err != nil {
				return nil, err
			}
			if !sameAgentsViewPrincipal(principal, current.principal) {
				return nil, agentsViewUnavailable()
			}
			return &api.AgentsViewServiceResponse{CacheControl: "private, no-store", Body: result}, nil
		})
}

func agentsViewRelayGenerationMatches(principal federationPrincipal, projectID int64, protocolVersion int, bindingUID, peerInstanceUID string, resetEpoch int64) bool {
	return projectID > 0 && principal.ScopedProjectID == projectID && principal.EnrollmentID > 0 &&
		protocolVersion == db.RelayProtocolVersion && principal.RelayProtocolVersion == db.RelayProtocolVersion &&
		uid.Valid(bindingUID) && principal.RelayBindingUID == bindingUID &&
		uid.Valid(peerInstanceUID) && principal.SpokeInstanceUID == peerInstanceUID &&
		resetEpoch > 0 && principal.RelayResetEpoch == resetEpoch
}

func sameAgentsViewPrincipal(before, after federationPrincipal) bool {
	return before == after
}

package daemon

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

const cronOccurrenceClaimsCapability = "cron_occurrence_claims_v1"

func registerCronClaimHandlers(h huma.API, cfg ServerConfig) {
	base := "/api/v1/projects/{project_id}/cron"
	huma.Register(h, huma.Operation{OperationID: "claimCronOccurrence", Method: http.MethodPost, Path: base + "/jobs/{job_uid}/claims", DefaultStatus: 201, Responses: map[string]*huma.Response{"200": {Description: "Retained grant replay", Content: map[string]*huma.MediaType{"application/json": {Schema: h.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[api.CronClaimResponseBody](), true, "")}}}}, MaxBodyBytes: 8192, Errors: []int{400, 401, 403, 404, 409, 503}}, func(ctx context.Context, in *api.ClaimCronOccurrenceRequest) (*api.CronClaimResponse, error) {
		ctx, principal, err := resolveCapabilityPrincipal(ctx, cfg, in.ProjectID, in.Authorization, api.ClaimActionBody{Holder: in.Body.Actor}, federationTransportOperation("claimCronOccurrence"), true, "claim", false)
		if err != nil {
			return nil, err
		}
		if !principal.Enrollment {
			var actor string
			ctx, actor, err = cronWriteContext(ctx, in.Body.Actor)
			if err != nil {
				return nil, err
			}
			principal.Holder = actor
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		origin, err := cronClaimOrigin(cfg, principal, in.Body.Origin)
		if err != nil {
			return nil, err
		}
		if in.Hops >= 16 {
			return nil, cronClaimPending()
		}
		finish, err := beginClaimFederationTransport(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, err
		}
		defer finish()
		binding, forward, err := cronClaimBinding(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if forward {
			return forwardCronClaim(ctx, cfg, binding, principal, origin, in.JobUID, in.Body, in.Hops)
		}
		if in.Body.ExpectedAuthorityInstanceUID != "" && in.Body.ExpectedAuthorityInstanceUID != cfg.DB.InstanceUID() {
			return nil, cronClaimPending()
		}

		result, err := cfg.DB.ClaimCronOccurrence(ctx, db.CronOccurrenceClaim{ProjectID: in.ProjectID, JobUID: in.JobUID, RunUID: in.Body.RunUID, DefinitionEventUID: in.Body.DefinitionEventUID, OccurrenceKey: in.Body.OccurrenceKey, HolderInstanceUID: origin.HolderInstanceUID, Actor: origin.Actor, Teammate: in.Body.Teammate, ExecutorLabel: in.Body.ExecutorLabel, WorkflowUID: in.Body.WorkflowUID, WorkflowDefinitionEventUID: in.Body.WorkflowDefinitionEventUID})
		if err != nil {
			return nil, cronClaimAPIError(ctx, cfg, in.ProjectID, result, err)
		}
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		return &api.CronClaimResponse{Status: status, Body: api.CronClaimResponseBody{Claimed: true, Replayed: result.Replayed, Claim: result.Claim}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "showCronOccurrenceClaim", Method: http.MethodGet, Path: base + "/claims/{run_uid}", Errors: []int{401, 403, 404, 503}}, func(ctx context.Context, in *api.CronClaimRequest) (*api.CronClaimResponse, error) {
		ctx, err := authorizeCronClaimRead(ctx, cfg, in.ProjectID, in.Authorization, "showCronOccurrenceClaim")
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		if in.Hops >= 16 {
			return nil, cronClaimPending()
		}
		finish, err := beginClaimFederationTransport(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, err
		}
		defer finish()
		binding, forward, err := cronClaimBinding(ctx, cfg, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if forward {
			return forwardCronClaimLookup(ctx, cfg, binding, in.RunUID, in.Hops)
		}
		claim, err := cfg.DB.CronOccurrenceClaim(ctx, in.ProjectID, in.RunUID)
		if err != nil {
			return nil, cronClaimAPIError(ctx, cfg, in.ProjectID, db.CronClaimResult{}, err)
		}
		return &api.CronClaimResponse{Status: http.StatusOK, Body: api.CronClaimResponseBody{Claimed: true, Replayed: true, Claim: claim}}, nil
	})
}

// Read authentication preserves ordinary anonymous read-only capability discovery.
// Enrollment credentials are accepted only for this project's claim permission.
func authorizeCronClaimRead(ctx context.Context, cfg ServerConfig, p int64, authz, op string) (context.Context, error) {
	if _, ok := PrincipalFromContext(ctx); ok {
		return ctx, nil
	}
	if hasBearerHeader(authz) && !validLocalBearer(cfg.Auth.Token, authz) {
		authorized, _, err := authorizeFederationRequest(ctx, cfg, authz, p, "claim", federationTransportOperation(op))
		return authorized, err
	}
	if cfg.HostAccess != nil {
		return ctx, localAuthError(cfg, authz)
	}
	if cfg.Auth.Token != "" && !validLocalBearer(cfg.Auth.Token, authz) {
		return ctx, localAuthError(cfg, authz)
	}
	return ctx, nil
}

func cronClaimAPIError(ctx context.Context, cfg ServerConfig, p int64, result db.CronClaimResult, err error) error {
	switch {
	case errors.Is(err, db.ErrCronOccurrenceClaimed):
		return api.NewError(409, "cron_occurrence_claimed", err.Error(), "skip this occurrence", map[string]any{"claim": result.Claim})
	case errors.Is(err, db.ErrCronClaimIdentity):
		return api.NewError(409, "cron_claim_identity_conflict", err.Error(), "reuse the original immutable request", nil)
	case errors.Is(err, db.ErrCronClaimPending):
		return api.NewError(503, "cron_claim_pending", err.Error(), "keep pending; do not launch", nil)
	default:
		return cronAPIError(ctx, cfg, err, p, "", true)
	}
}

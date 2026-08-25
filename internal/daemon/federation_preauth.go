package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
)

const federationIngestPathSuffix = "/federation/events:ingest"

// withFederationIngestPreauthorization authenticates the large body-bearing
// federation route before Huma can read or decode its request body.
func withFederationIngestPreauthorization(cfg ServerConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		projectID, matched, valid := federationIngestProjectID(r.Method, r.URL.Path)
		if !matched {
			next.ServeHTTP(w, r)
			return
		}
		if !valid {
			api.WriteEnvelope(w, http.StatusBadRequest, "validation", "project_id must be a positive integer")
			return
		}
		operation := federationTransportOperation("ingestFederationProjectEvents")
		ctx, err := preauthorizeHostFederationIngest(r.Context(), cfg.HostAccess, projectID)
		if err != nil {
			writeFederationPreauthorizationError(w, err)
			return
		}
		authHeader := r.Header.Get("Authorization")
		authorization, err := evaluateFederationRequest(
			ctx, cfg, authHeader, projectID, "push", operation,
		)
		if err != nil {
			writeFederationPreauthorizationError(w, err)
			return
		}
		ctx = withFederationAuthorization(
			ctx, authHeader, projectID, "push", operation, authorization,
		)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func preauthorizeHostFederationIngest(
	ctx context.Context,
	controller HostAccessController,
	projectID int64,
) (context.Context, error) {
	if controller == nil {
		return ctx, nil
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return ctx, nil
	}
	if !validHostPrincipal(principal) {
		return ctx, api.NewError(http.StatusUnauthorized, "authentication_required",
			"authentication required", "", nil)
	}
	policy, ok := hostOperationPolicy("ingestFederationProjectEvents")
	if !ok {
		return ctx, api.NewError(http.StatusServiceUnavailable, "access_unavailable",
			"access decision unavailable", "", nil)
	}
	projectIDText := strconv.FormatInt(projectID, 10)
	request := HostAccessRequest{
		Subject: principal.Subject,
		Actor:   principal.Actor,
		Operation: HostOperation{
			ID: "ingestFederationProjectEvents", Method: http.MethodPost,
			Path:       "/api/v1/projects/{project_id}/federation/events:ingest",
			PathParams: map[string]string{"project_id": projectIDText},
			ProjectIDs: []int64{projectID}, Policy: policy,
		},
	}
	decision, err := controller.Authorize(ctx, request)
	if errors.Is(err, ErrHostAccessDenied) {
		return ctx, api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
	}
	if err != nil || decision.TransactionFence == nil {
		return ctx, api.NewError(http.StatusServiceUnavailable, "access_unavailable",
			"access decision unavailable", "", nil)
	}
	state := &hostAccessState{
		controller: controller, request: request, decision: decision, authorized: true,
	}
	return context.WithValue(ctx, hostAccessStateContextKey{}, state), nil
}

// federationIngestProjectID is a deliberate exception to the route matcher in
// auth_routes.go, not drift. It runs ahead of the huma mux so credential and
// path validation happen before Huma may read a 64 MiB body, and it needs two
// things the matcher collapses: the parsed project id, and the "matched the
// route but the id is unusable" case that answers 400 rather than falling
// through. TestFederationIngestPreauthParserMatchesRegisteredRoute pins it
// against the registered ingest route so a path rename fails a test.
func federationIngestProjectID(method, path string) (projectID int64, matched, valid bool) {
	if method != http.MethodPost {
		return 0, false, false
	}
	rest, ok := strings.CutPrefix(path, "/api/v1/projects/")
	if !ok {
		return 0, false, false
	}
	projectIDText, ok := strings.CutSuffix(rest, federationIngestPathSuffix)
	if !ok || strings.Contains(projectIDText, "/") {
		return 0, false, false
	}
	projectID, err := strconv.ParseInt(projectIDText, 10, 64)
	if err != nil || projectID <= 0 {
		return 0, true, false
	}
	return projectID, true, true
}

func writeFederationPreauthorizationError(w http.ResponseWriter, err error) {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		if apiErr.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "5")
		}
		api.WriteEnvelope(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "federation authentication failed")
}

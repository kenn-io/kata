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

type federationPreauthorizationKey struct{}

type federationPreauthorization struct {
	authorization string
	projectID     int64
	capability    string
	operation     HostFederationOperation
	principal     federationPrincipal
}

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
		operation := HostFederationOperation{ID: "ingestFederationProjectEvents", Mutation: true}
		ctx, principal, err := authorizeFederationRequest(
			r.Context(), cfg, r.Header.Get("Authorization"), projectID, "push", operation,
		)
		if err != nil {
			writeFederationPreauthorizationError(w, err)
			return
		}
		ctx = context.WithValue(ctx, federationPreauthorizationKey{}, federationPreauthorization{
			authorization: r.Header.Get("Authorization"),
			projectID:     projectID,
			capability:    "push",
			operation:     operation,
			principal:     principal,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

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
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		api.WriteEnvelope(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	api.WriteEnvelope(w, http.StatusInternalServerError, "internal", "federation authentication failed")
}

func federationPreauthorizationFromContext(
	ctx context.Context,
	authorization string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
) (federationPrincipal, bool) {
	preauthorized, ok := ctx.Value(federationPreauthorizationKey{}).(federationPreauthorization)
	if !ok || preauthorized.authorization != authorization || preauthorized.projectID != projectID ||
		preauthorized.capability != capability || preauthorized.operation != operation {
		return federationPrincipal{}, false
	}
	return preauthorized.principal, true
}

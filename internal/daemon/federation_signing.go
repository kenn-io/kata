package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/federationsigning"
)

// These operation IDs are the explicit restricted-ingress allowlist. Paths
// come from native registration so ingress cannot drift from actual routes.
var signingCapabilities = map[string]string{
	"getFederationProjectMetadata":  "pull",
	"pollFederationProjectEvents":   "pull",
	"ingestFederationProjectEvents": "push",
	"disconnectRelayEnrollment":     "push",
	"getRelayReset":                 "pull",
	"offerRelayDeliveries":          "pull",
	"acceptRelayDeliveries":         "push",
	"ackRelayDeliveries":            "pull",
	"claimIssue":                    "claim",
	"acquireIssueLease":             "claim",
	"renewIssueLease":               "claim",
	"releaseIssueLease":             "claim",
	"getIssueLeaseStatus":           "claim",
}

type signingRoute struct {
	projectID  int64
	method     string
	operation  string
	capability string
}
type signingRouteKey struct{}

func (s *Server) withFederationSigning(next http.Handler, ingress bool) http.Handler {
	matcher := http.NewServeMux()
	for id, route := range registeredOperations(s.api.OpenAPI()) {
		capability, ok := signingCapabilities[id]
		if !ok {
			continue
		}
		matcher.HandleFunc(route.Method+" "+route.Path, func(_ http.ResponseWriter, r *http.Request) {
			result := r.Context().Value(signingRouteKey{}).(*signingRoute)
			idValue, err := strconv.ParseInt(r.PathValue("project_id"), 10, 64)
			if err == nil && idValue > 0 {
				// ServeMux also dispatches HEAD through GET patterns. Preserve the
				// route so required signing applies to that request as well.
				*result = signingRoute{projectID: idValue, method: route.Method, operation: id, capability: capability}
			}
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var route signingRoute
		matcher.ServeHTTP(discardResponseWriter{}, r.WithContext(context.WithValue(r.Context(), signingRouteKey{}, &route)))
		if route.operation == "" {
			if ingress {
				http.NotFound(w, r)
			} else {
				next.ServeHTTP(w, r)
			}
			return
		}
		if ingress && r.Method != route.method {
			http.NotFound(w, r)
			return
		}
		supplied := len(r.Header.Values("Signature")) != 0 || len(r.Header.Values("Signature-Input")) != 0 || len(r.Header.Values("Content-Digest")) != 0
		if !ingress && !supplied {
			if !s.cfg.FederationSigningRequired {
				next.ServeHTTP(w, r)
				return
			}
			// Owner/identity lease operations remain ordinary daemon operations.
			// Ingress never takes this local-authority path.
			if route.capability == "claim" {
				if _, ok := PrincipalFromContext(r.Context()); ok || validLocalBearer(s.cfg.Auth.Token, r.Header.Get("Authorization")) || !hasBearerHeader(r.Header.Get("Authorization")) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		// Bulk uploads retain their memory bound without occupying the slots
		// used by metadata, event polls, and lease requests.
		admission := s.federationControlAdmission
		bodyLimit := int64(64 << 10)
		if route.operation == "ingestFederationProjectEvents" || route.operation == "acceptRelayDeliveries" {
			admission = s.federationIngestAdmission
			bodyLimit = federationsigning.MaxBodyBytes
		}
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		default:
			w.Header().Set("Retry-After", "1")
			api.WriteEnvelope(w, http.StatusTooManyRequests, "admission_limited", "federation admission is full")
			return
		}
		deadline := time.Now().Add(60 * time.Second)
		_ = http.NewResponseController(w).SetReadDeadline(deadline)
		_ = http.NewResponseController(w).SetWriteDeadline(deadline)
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		if r.ContentLength > bodyLimit {
			api.WriteEnvelope(w, http.StatusRequestEntityTooLarge, "body_too_large", "federation body is too large")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
		headerSize := 0
		for name, values := range r.Header {
			for _, value := range values {
				headerSize += len(name) + len(value) + 4
			}
		}
		if headerSize > federationsigning.MaxHeaderBytes || len(r.Header.Values("Authorization")) != 1 {
			api.WriteEnvelope(w, http.StatusUnauthorized, "auth_invalid", "invalid federation authorization")
			return
		}
		auth := r.Header.Get("Authorization")
		operation := federationTransportOperation(route.operation)
		authorization, err := evaluateFederationRequest(ctx, s.cfg, auth, route.projectID, route.capability, operation)
		if err != nil {
			writeFederationPreauthorizationError(w, err)
			return
		}
		if ingress && authorization.principal.ScopedProjectID != route.projectID {
			api.WriteEnvelope(w, http.StatusForbidden, "auth_invalid", "project-scoped enrollment required")
			return
		}
		if ingress || supplied || s.cfg.FederationSigningRequired {
			if s.cfg.FederationSigning == nil {
				api.WriteEnvelope(w, http.StatusServiceUnavailable, "signing_unavailable", "federation signing unavailable")
				return
			}
			r = r.WithContext(ctx)
			err = s.cfg.FederationSigning.Verify(r, authorization.principal.EnrollmentID)
			if err != nil {
				status := http.StatusUnauthorized
				if errors.Is(err, federationsigning.ErrWarming) || errors.Is(err, federationsigning.ErrKey) || errors.Is(err, federationsigning.ErrState) {
					status = http.StatusServiceUnavailable
				}
				if errors.Is(err, federationsigning.ErrCapacity) {
					status = http.StatusTooManyRequests
					w.Header().Set("Retry-After", "1")
				}
				if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
					api.WriteEnvelope(w, http.StatusRequestEntityTooLarge, "body_too_large", "federation body is too large")
					return
				}
				api.WriteEnvelope(w, status, "signature_invalid", "federation signature rejected")
				return
			}
			// Revocation during body upload must also stop reads. Mutations retain
			// their native transaction fence through the cached authorization.
			if _, err := s.cfg.DB.AuthorizeFederationToken(ctx, auth[len(authBearerPrefix):], route.projectID, route.capability); err != nil {
				api.WriteEnvelope(w, http.StatusForbidden, "auth_invalid", "federation enrollment revoked")
				return
			}
		}
		ctx = withFederationAuthorization(ctx, auth, route.projectID, route.capability, operation, authorization)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

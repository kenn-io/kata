package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
)

// These operation IDs are the explicit restricted-ingress allowlist. Paths
// come from native registration so ingress cannot drift from actual routes.
var signingCapabilities = map[string][]string{
	"createCronJob":                 {"push"},
	"replaceCronJob":                {"push"},
	"archiveCronJob":                {"push"},
	"restoreCronJob":                {"push"},
	"getFederationProjectMetadata":  {"pull", "push"},
	"pollFederationProjectEvents":   {"pull"},
	"watchFederationProjectEvents":  {"pull"},
	"searchFederationProject":       {"pull"},
	"ingestFederationProjectEvents": {"push"},
	"disconnectRelayEnrollment":     {"push"},
	"getRelayReset":                 {"pull"},
	"offerRelayDeliveries":          {"pull"},
	"acceptRelayDeliveries":         {"push"},
	"ackRelayDeliveries":            {"pull"},
	"claimIssue":                    {"claim"},
	"acquireIssueLease":             {"claim"},
	"renewIssueLease":               {"claim"},
	"releaseIssueLease":             {"claim"},
	"getIssueLeaseStatus":           {"claim"},
	"uploadFederatedDocbank":        {"push"},
	"linkFederatedDocbank":          {"push"},
	"federatedDocbankCapabilities":  {"pull", "push"},
	"federatedDocbankContent":       {"pull"},
}

func isCronJobWriteOperation(operation string) bool {
	switch operation {
	case "createCronJob", "replaceCronJob", "archiveCronJob", "restoreCronJob":
		return true
	default:
		return false
	}
}

type signingRoute struct {
	projectID    int64
	method       string
	operation    string
	capabilities []string
}
type signingRouteKey struct{}

func (s *Server) withFederationSigning(next http.Handler, ingress bool) http.Handler {
	matcher := http.NewServeMux()
	for id, route := range registeredOperations(s.api.OpenAPI()) {
		capabilities, ok := signingCapabilities[id]
		if !ok {
			continue
		}
		matcher.HandleFunc(route.Method+" "+route.Path, func(_ http.ResponseWriter, r *http.Request) {
			result := r.Context().Value(signingRouteKey{}).(*signingRoute)
			idValue, err := strconv.ParseInt(r.PathValue("project_id"), 10, 64)
			if err == nil && idValue > 0 {
				// ServeMux also dispatches HEAD through GET patterns. Preserve the
				// route so required signing applies to that request as well.
				*result = signingRoute{projectID: idValue, method: route.Method, operation: id, capabilities: capabilities}
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
		push := route.operation == "ingestFederationProjectEvents" || route.operation == "acceptRelayDeliveries" || isCronJobWriteOperation(route.operation)
		supplied := len(r.Header.Values("Signature")) != 0 || len(r.Header.Values("Signature-Input")) != 0
		if !ingress && !supplied {
			// Without required signing, unsigned requests are ordinary daemon
			// operations, as are owner/identity lease and cron writes. Ingress never
			// takes this local-authority path.
			local := !s.cfg.FederationSigningRequired
			if !local && (slices.Contains(route.capabilities, "claim") || isCronJobWriteOperation(route.operation)) {
				_, ok := PrincipalFromContext(r.Context())
				local = ok || validLocalBearer(s.cfg.Auth.Token, r.Header.Get("Authorization")) || !hasBearerHeader(r.Header.Get("Authorization"))
			}
			if local {
				// Ingest preauthorization charges unsigned event pushes after it
				// authorizes them. Local cron writes use their ordinary handler.
				next.ServeHTTP(w, r)
				return
			}
		}
		bodyLimit := int64(64 << 10)
		attachmentTransfer := signingpolicy.Unambiguous(r.URL) && federationsigning.IsAttachmentTransfer(r.Method, r.URL.Path)
		if push {
			bodyLimit = federationsigning.MaxBodyBytes
		} else if attachmentTransfer {
			admission := s.federationReadAdmission
			if r.Method == http.MethodPost {
				bodyLimit = federationsigning.AttachmentMaxBodyBytes
				admission = s.federationTransferAdmission
			}
			select {
			case admission <- struct{}{}:
				defer func() { <-admission }()
			default:
				w.Header().Set("Retry-After", "1")
				api.WriteEnvelope(w, http.StatusTooManyRequests, "admission_limited", "federation admission is full")
				return
			}
		}
		deadline := time.Now().Add(60 * time.Second)
		if attachmentTransfer {
			deadline = time.Now().Add(federationsigning.AttachmentTransferTimeout)
		}
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
		relayCleanup := route.operation == "disconnectRelayEnrollment"
		var authorization federationAuthorization
		var err error
		if relayCleanup {
			authorization, err = evaluateRelaySelfRevocation(ctx, s.cfg, auth, route.projectID)
		} else {
			authorization, err = evaluateFederationRequestForCapabilities(ctx, s.cfg, auth, route.projectID, route.capabilities, operation)
		}
		if err != nil {
			writeFederationPreauthorizationError(w, err)
			return
		}
		if ingress && authorization.principal.ScopedProjectID != route.projectID {
			api.WriteEnvelope(w, http.StatusForbidden, "auth_invalid", "project-scoped enrollment required")
			return
		}
		if push {
			// Verification reads the body through this charge.
			body := &federationPushBody{ReadCloser: r.Body, memory: s.federationPushMemory}
			defer body.release()
			r.Body = body
		}
		if ingress || supplied || s.cfg.FederationSigningRequired {
			if s.cfg.FederationSigning == nil {
				api.WriteEnvelope(w, http.StatusServiceUnavailable, "signing_unavailable", "federation signing unavailable")
				return
			}
			if !supplied {
				// Older bridges treat other 401, 403, and 404 responses as revoked grants.
				api.WriteEnvelope(w, http.StatusPreconditionRequired, "hub_signing_required", "hub requires signed requests")
				return
			}
			r = r.WithContext(ctx)
			var admission federationsigning.Admission
			admission, err = s.cfg.FederationSigning.Verify(r, authorization.principal.EnrollmentID)
			if err != nil {
				if errors.Is(err, errFederationPushMemory) {
					writeFederationPushMemoryFull(w)
					return
				}
				// Warm-up is not a signature fault: tell the spoke to retry
				// rather than to check its keys and clock.
				if errors.Is(err, federationsigning.ErrWarming) {
					w.Header().Set("Retry-After", "5")
					api.WriteEnvelope(w, http.StatusServiceUnavailable, "signing_warming",
						"federation signing is warming up replay protection; retry later")
					return
				}
				status := http.StatusUnauthorized
				if errors.Is(err, signingpolicy.ErrKey) || errors.Is(err, federationsigning.ErrState) {
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
			if attachmentTransfer {
				// Verify replaced the wire body with a verified disk spool. Keep it
				// alive through the handler, including final authorization failures.
				body := r.Body
				defer func() { _ = body.Close() }()
			}
			ctx = withFederationSigningAdmission(ctx, admission)
			// Revocation during body upload must also stop reads. Mutations retain
			// their native transaction fence through the cached authorization.
			if !relayCleanup {
				if _, err := s.cfg.DB.AuthorizeFederationToken(ctx, auth[len(authBearerPrefix):], route.projectID, authorization.capability); err != nil {
					api.WriteEnvelope(w, http.StatusForbidden, "auth_invalid", "federation enrollment revoked")
					return
				}
			}
		}
		if relayCleanup {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		ctx = withFederationAuthorization(ctx, auth, route.projectID, strings.Join(route.capabilities, ","), operation, authorization)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bufferFederationPush reads an authorized unsigned push body through the
// shared push budget before the handler decodes it.
func (s *Server) bufferFederationPush(w http.ResponseWriter, r *http.Request) (*federationPushBody, bool) {
	if r.ContentLength > federationsigning.MaxBodyBytes {
		api.WriteEnvelope(w, http.StatusRequestEntityTooLarge, "body_too_large", "federation body is too large")
		return nil, false
	}
	// Match Huma's default body-read timeout, which normally starts after this middleware.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	body := &federationPushBody{ReadCloser: http.MaxBytesReader(w, r.Body, federationsigning.MaxBodyBytes), memory: s.federationPushMemory}
	data, err := io.ReadAll(body)
	if err != nil {
		// Keep the deadline on errors so the server cannot block draining the body.
		body.release()
		if errors.Is(err, errFederationPushMemory) {
			writeFederationPushMemoryFull(w)
		} else if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			api.WriteEnvelope(w, http.StatusRequestEntityTooLarge, "body_too_large", "federation body is too large")
		} else {
			api.WriteEnvelope(w, http.StatusBadRequest, "body_invalid", "federation body could not be read")
		}
		return nil, false
	}
	_ = controller.SetReadDeadline(time.Time{})
	r.Body = io.NopCloser(bytes.NewReader(data))
	return body, true
}

var errFederationPushMemory = errors.New("federation push memory is full")

type federationPushBody struct {
	io.ReadCloser
	memory *semaphore.Weighted
	held   int64
}

func (b *federationPushBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if !b.memory.TryAcquire(int64(n)) {
			return 0, errFederationPushMemory
		}
		b.held += int64(n)
	}
	return n, err
}

func (b *federationPushBody) release() { b.memory.Release(b.held) }

func writeFederationPushMemoryFull(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	api.WriteEnvelope(w, http.StatusTooManyRequests, "admission_limited", "federation push memory is full")
}

type federationSigningAdmissionContextKey struct{}

// withFederationSigningAdmission records the key that verified the request's
// signature, so a request held open can check that key again before it
// releases data.
func withFederationSigningAdmission(ctx context.Context, admission federationsigning.Admission) context.Context {
	return context.WithValue(ctx, federationSigningAdmissionContextKey{}, admission)
}

// recheckFederationSigningKey fails when the key that verified a signed
// request has since expired or had its secret replaced. Unsigned requests
// pass.
func recheckFederationSigningKey(ctx context.Context) error {
	admission, signed := ctx.Value(federationSigningAdmissionContextKey{}).(federationsigning.Admission)
	if !signed {
		return nil
	}
	err := admission.Current()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, signingpolicy.ErrKey):
		return api.NewError(http.StatusServiceUnavailable, "signature_invalid", "federation signature rejected", "", nil)
	default:
		// Answer as verification answers a key that is already retired.
		return api.NewError(http.StatusUnauthorized, "signature_invalid", "federation signature rejected", "", nil)
	}
}

func evaluateRelaySelfRevocation(
	ctx context.Context,
	cfg ServerConfig,
	authHeader string,
	projectID int64,
) (federationAuthorization, error) {
	if !strings.HasPrefix(authHeader, authBearerPrefix) {
		return federationAuthorization{}, api.NewError(http.StatusUnauthorized, "auth_required", "Authorization bearer required", "", nil)
	}
	token := strings.TrimPrefix(authHeader, authBearerPrefix)
	if token == "" {
		return federationAuthorization{}, api.NewError(http.StatusUnauthorized, "auth_required", "Authorization bearer required", "", nil)
	}
	resolver, ok := cfg.DB.(db.RelaySelfRevocationAuthenticator)
	if !ok {
		return federationAuthorization{}, api.NewError(http.StatusServiceUnavailable,
			"access_unavailable", "relay cleanup authentication unavailable", "", nil)
	}
	enrollmentID, err := resolver.RelaySelfRevocationEnrollmentID(ctx, token, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return federationAuthorization{}, federationCredentialDenied()
	}
	if err != nil {
		return federationAuthorization{}, internalAPIError(err)
	}
	return federationAuthorization{principal: federationPrincipal{
		EnrollmentID: enrollmentID, ScopedProjectID: projectID,
	}}, nil
}

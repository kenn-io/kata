package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/hooks"
)

// ServerConfig wires the daemon's runtime dependencies. DB and StartedAt are
// required; Endpoint is only consulted by Run; Broadcaster is owned by the
// server (NewServer fills it if nil so handler tests don't have to plumb one
// through). Hooks is optional and defaults to hooks.NewNoop() when nil so
// mutation handlers can fan out events unconditionally.
type ServerConfig struct {
	DB             db.Storage
	StartedAt      time.Time
	Endpoint       DaemonEndpoint
	Broadcaster    *EventBroadcaster
	FederationWake func()
	Hooks          hooks.Sink
	// CloseThrottle controls whether the sibling-burst and repeated-
	// message guards run on close. Zero-value (Enabled=false) is taken
	// as "guards on" so handler tests and existing test harness setups
	// keep the v1 default behavior without plumbing the policy through;
	// the daemon entry point sets ThrottleDisabled=true only when the
	// operator opts out via [close.throttle] in config.toml.
	CloseThrottle CloseThrottlePolicy

	// Auth carries the bearer-token policy resolved at daemon start.
	// Token == "" disables bearer auth (appropriate for Unix-socket and
	// loopback-TCP deployments).
	Auth config.AuthConfig

	// InsecureReadonly permits unauthenticated GETs on non-loopback TCP
	// even when Auth.Token == "". DEV ONLY — not for production.
	InsecureReadonly bool

	// Logger is the structured logger middleware uses for operator-visible
	// warnings (currently: trusted-proxy header overwriting an upstream
	// bearer-derived principal). Nil uses slog.Default(); tests inject a
	// per-test logger so output is observable and isolated.
	Logger *slog.Logger
}

// authPolicy returns the resolved bearer-auth policy in the form the
// middleware consumes. Keeping the conversion here means the middleware
// stays unaware of ServerConfig and config.AuthConfig.
func (c ServerConfig) authPolicy() authPolicy {
	return authPolicy{
		Token:                c.Auth.Token,
		TrustPrivateNetwork:  c.Auth.TrustPrivateNetwork,
		InsecureReadonly:     c.InsecureReadonly,
		RequireTokenIdentity: c.Auth.RequireTokenIdentity,
	}
}

// CloseThrottlePolicy is the runtime form of [close.throttle] in
// <KATA_HOME>/config.toml. Using ThrottleDisabled (rather than Enabled)
// makes the zero value match the v1 default: guards on.
type CloseThrottlePolicy struct {
	ThrottleDisabled bool
}

// Server bundles the http handler and lifecycle.
type Server struct {
	cfg     ServerConfig
	handler http.Handler
	api     huma.API
}

// NewServer wires routes onto a fresh http.ServeMux. The returned handler is
// safe to mount in tests via httptest.NewServer.
func NewServer(cfg ServerConfig) *Server {
	api.InstallErrorFormatter()
	if cfg.Broadcaster == nil {
		cfg.Broadcaster = NewEventBroadcaster()
	}
	if cfg.Hooks == nil {
		cfg.Hooks = hooks.NewNoop()
	}

	mux := http.NewServeMux()
	humaConfig := huma.DefaultConfig("kata", "0.1.0")
	humaConfig.OpenAPIPath = "" // Plan 1: no /openapi.json
	// Drop DefaultConfig's SchemaLinkTransformer: it rebuilds response structs
	// via reflection (adding a $schema field), which silently bypasses any
	// MarshalJSON. Our APIError relies on MarshalJSON to emit the wire-spec
	// envelope shape, so we must disable the transform.
	humaConfig.CreateHooks = nil
	humaAPI := humago.New(mux, humaConfig)

	s := &Server{cfg: cfg, api: humaAPI}
	registerRoutes(humaAPI, mux, cfg)

	s.handler = withCSRFGuards(requireBearer(cfg.authPolicy(), cfg.DB)(withTrustedProxyActor(cfg)(mux)))
	return s
}

// Handler returns the http.Handler suitable for httptest.NewServer.
func (s *Server) Handler() http.Handler { return s.handler }

// API returns the underlying huma.API for handler registration in tests.
func (s *Server) API() huma.API { return s.api }

// Close releases server-owned resources. Currently a no-op since the DB is
// owned by the caller.
func (s *Server) Close() error { return nil }

// Run listens on the configured endpoint until ctx is cancelled. The caller is
// responsible for writing the runtime file once Run has started.
func (s *Server) Run(ctx context.Context) error {
	if s.cfg.Endpoint == nil {
		return errors.New("server: endpoint is required for Run")
	}
	l, err := s.cfg.Endpoint.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ctx, l)
}

// Serve runs the http server on the provided listener until ctx is cancelled.
// Useful for tests that bind their own loopback listener (avoiding the
// listener-close-then-reopen TOCTOU window).
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	httpSrv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// BaseContext roots every request in the daemon ctx so long-lived
		// SSE handlers exit on Shutdown via r.Context().Done().
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if err := httpSrv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// withCSRFGuards rejects browser-borne requests and enforces JSON content type
// on mutation methods that carry a body. Per spec §2.9, CLI/TUI never set
// Origin so this is transparent for our own clients. Errors are emitted as
// JSON envelopes matching api.ErrorEnvelope so the wire contract holds for
// every non-2xx response, not just handler-returned ones.
func withCSRFGuards(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			api.WriteEnvelope(w, http.StatusForbidden, "origin_forbidden",
				"Origin header forbidden")
			return
		}
		if isMutation(r.Method) && r.ContentLength != 0 {
			ct := r.Header.Get("Content-Type")
			if !strings.HasPrefix(ct, "application/json") {
				api.WriteEnvelope(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
					"Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// isMutation reports whether the HTTP method modifies state and therefore
// should be subject to the JSON content-type guard.
func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// registerRoutes installs the per-resource handler groups onto humaAPI. Each
// group lives in its own file (handlers_health.go, handlers_projects.go, etc.)
// and replaces the matching stub below as it lands. The events handler also
// receives mux so it can register the SSE endpoint as a raw http.HandlerFunc
// (Huma doesn't model streaming responses).
func registerRoutes(humaAPI huma.API, mux *http.ServeMux, cfg ServerConfig) {
	registerHealth(humaAPI, cfg)
	registerInstanceHandlers(humaAPI, cfg)
	registerTokenHandlers(humaAPI, cfg)
	registerProjects(humaAPI, cfg)
	registerIssues(humaAPI, cfg)
	registerImportsHandlers(humaAPI, cfg)
	registerComments(humaAPI, cfg)
	registerActions(humaAPI, cfg)
	registerLinks(humaAPI, cfg)
	registerLabels(humaAPI, cfg)
	registerOwnership(humaAPI, cfg)
	registerPriority(humaAPI, cfg)
	registerReady(humaAPI, cfg)
	registerSearch(humaAPI, cfg)
	registerDestructive(humaAPI, cfg)
	registerRecurrences(humaAPI, cfg)
	registerMetadata(humaAPI, cfg)
	registerMove(humaAPI, cfg)
	registerEventsHandlers(humaAPI, mux, cfg)
	registerFederationHandlers(humaAPI, cfg)
	registerClaimHandlers(humaAPI, cfg)
	registerDigestHandlers(humaAPI, cfg)
	registerAuditHandlers(humaAPI, cfg)
}

// registerHealth registers /api/v1/ping and /api/v1/health.
func registerHealth(humaAPI huma.API, cfg ServerConfig) {
	registerHealthHandlers(humaAPI, cfg)
}

// registerProjects registers project-scoped routes (resolve, init, list, show).
func registerProjects(humaAPI huma.API, cfg ServerConfig) {
	registerProjectsHandlers(humaAPI, cfg)
}

// registerIssues registers issue CRUD routes (create/list/show/edit).
func registerIssues(humaAPI huma.API, cfg ServerConfig) {
	registerIssuesHandlers(humaAPI, cfg)
}

// registerComments registers issue-comment routes.
func registerComments(humaAPI huma.API, cfg ServerConfig) {
	registerCommentsHandlers(humaAPI, cfg)
}

// registerActions registers close/reopen action routes.
func registerActions(humaAPI huma.API, cfg ServerConfig) {
	registerActionsHandlers(humaAPI, cfg)
}

// registerLinks registers POST/DELETE /links routes.
func registerLinks(humaAPI huma.API, cfg ServerConfig) {
	registerLinksHandlers(humaAPI, cfg)
}

// registerLabels registers POST/DELETE /labels and GET /labels (counts) routes.
func registerLabels(humaAPI huma.API, cfg ServerConfig) {
	registerLabelsHandlers(humaAPI, cfg)
}

// registerOwnership registers POST /actions/assign and /actions/unassign routes.
func registerOwnership(humaAPI huma.API, cfg ServerConfig) {
	registerOwnershipHandlers(humaAPI, cfg)
}

// registerPriority registers POST /actions/priority.
func registerPriority(humaAPI huma.API, cfg ServerConfig) {
	registerPriorityHandlers(humaAPI, cfg)
}

// registerReady registers GET /projects/{id}/ready.
func registerReady(humaAPI huma.API, cfg ServerConfig) {
	registerReadyHandlers(humaAPI, cfg)
}

// registerSearch registers GET /projects/{id}/search.
func registerSearch(humaAPI huma.API, cfg ServerConfig) {
	registerSearchHandlers(humaAPI, cfg)
}

// registerDestructive registers /actions/delete, /actions/restore, /actions/purge.
func registerDestructive(humaAPI huma.API, cfg ServerConfig) {
	registerDestructiveHandlers(humaAPI, cfg)
}

// registerRecurrences registers the recurrence CRUD routes.
func registerRecurrences(humaAPI huma.API, cfg ServerConfig) {
	registerRecurrencesHandlers(humaAPI, cfg)
}

// registerMetadata registers metadata patch routes.
func registerMetadata(humaAPI huma.API, cfg ServerConfig) {
	registerMetadataHandlers(humaAPI, cfg)
}

// registerMove registers the cross-project issue move action route.
func registerMove(humaAPI huma.API, cfg ServerConfig) {
	registerMoveHandlers(humaAPI, cfg)
}

// validateActor returns a 400 validation error when actor is empty after
// trimming whitespace. Huma's `required:"true"` only checks presence, so a
// blank or whitespace-only actor sneaks through to the DB and surfaces as a
// 500 from the events.actor / issue_labels.author CHECK constraint.
func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return api.NewError(400, "validation", "actor must be non-empty", "", nil)
	}
	return nil
}

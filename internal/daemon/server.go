package daemon

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/klauspost/compress/gzhttp"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/rootbridge"
	"go.kenn.io/kata/internal/vector"
	kataweb "go.kenn.io/kata/internal/web"
)

// ServerConfig wires the daemon's runtime dependencies. DB and StartedAt are
// required; Endpoint is only consulted by Run; Broadcaster is owned by the
// server (NewServer fills it if nil so handler tests don't have to plumb one
// through). Hooks is optional and defaults to hooks.NewNoop() when nil so
// mutation handlers can fan out events unconditionally.
type ServerConfig struct {
	DB db.Storage
	// UIStore supplies coherent browser projections. Nil defaults to DB when
	// the configured storage backend implements db.UIStore.
	UIStore db.UIStore
	// UIClock supplies the current time for browser readiness.
	// Nil uses time.Now. Tests inject it to verify timed schedule transitions.
	UIClock func() time.Time
	// DefaultTimezone applies to civil schedules without an issue-level
	// timezone. Empty preserves the UTC default.
	DefaultTimezone               string
	StartedAt                     time.Time
	Endpoint                      *kitdaemon.Endpoint
	Broadcaster                   *EventBroadcaster
	FederationWake                func()
	FederationCredentials         config.FederationCredentialStore
	FederationCatalog             []config.CatalogDaemonConfig
	FederationRebindFetchMetadata FederationRebindMetadataFetcher
	// DisableFederationRebind omits the rebind operation when the embedding
	// host does not expose a catalog and exact credential replacement.
	DisableFederationRebind  bool
	GitHubSyncFetcher        githubsync.Fetcher
	GitHubSyncConfig         config.GitHubSyncConfig
	GitHubSyncFetcherFactory func(config.GitHubSyncConfig) githubsync.Fetcher
	GitHubSyncRunnerFactory  GitHubSyncRunnerFactory
	GitHubSyncWake           func()
	Hooks                    hooks.Sink
	ExternalRootRegistry     *rootbridge.Registry
	ExternalRootService      *rootbridge.Service
	ExternalRootReconciler   *rootbridge.Reconciler
	ExternalRootWake         func(int64)
	// CloseThrottle controls whether the opt-in sibling-burst and repeated-
	// message guards run on close. Zero-value means "guards off".
	CloseThrottle CloseThrottlePolicy

	// Auth carries the bearer-token policy resolved at daemon start.
	// Token == "" disables bearer auth (appropriate for Unix-socket and
	// loopback-TCP deployments).
	Auth config.AuthConfig

	// WebSessions owns browser cookie+header session state. Nil keeps
	// the browser-only routes and middleware disabled for embedded/API-only use.
	WebSessions *WebSessionManager

	// WebDaemons is the client daemon catalog exposed to the first-class web
	// UI. Credentials remain server-side and are used only by the selected-
	// daemon proxy. ActiveWebDaemon selects the initial catalog entry.
	WebDaemons      []config.CatalogDaemonConfig
	ActiveWebDaemon string

	// InsecureReadonly permits unauthenticated GETs on non-loopback TCP
	// even when Auth.Token == "". DEV ONLY — not for production.
	InsecureReadonly bool

	// Logger is the structured logger middleware uses for operator-visible
	// warnings (currently: trusted-proxy header overwriting an upstream
	// bearer-derived principal). Nil uses slog.Default(); tests inject a
	// per-test logger so output is observable and isolated.
	Logger *slog.Logger

	// Embedder is the semantic-search embedding client. Nil means semantic
	// search is disabled and the search handler falls back to lexical-only.
	Embedder *embedding.Client

	// VectorIndex is the semantic-search sidecar index. Nil means semantic and
	// hybrid search are unavailable, exactly like a nil Embedder.
	VectorIndex *vector.Index

	// ReconcilerHealth snapshots the embedding reconciler's operator-visible
	// state for /health. Nil means semantic search is disabled, in which case
	// the health response omits the embeddings block entirely.
	ReconcilerHealth func() ReconcilerHealth

	// FederationConfigHealth snapshots declarative federation convergence for
	// /health. Nil means no mappings are configured, so the block is omitted.
	FederationConfigHealth func() api.FederationConfigHealth

	// HostAccess enables in-process authentication and authorization supplied
	// by a mounting application. It is nil for the standalone daemon.
	HostAccess HostAccessController

	// HostFederationAccess optionally adds host-owned authorization after Kata
	// authenticates a project-scoped federation enrollment.
	HostFederationAccess HostFederationAccessController

	// IdleAdmission admits ordinary HTTP requests as foreground daemon work.
	// Nil disables idle lifecycle handling.
	IdleAdmission IdleForegroundAdmission
	// IdleShutdownHealth snapshots effective idle state for /health. Nil omits the
	// capability, including on explicit or remotely exposed daemons.
	IdleShutdownHealth func() IdleSnapshot

	// EmbeddingProfile removes native administrative routes when their
	// lifecycle is owned by a mounting application. The standalone daemon uses
	// the zero value and retains every route.
	EmbeddingProfile EmbeddingProfile
}

// authPolicy returns the resolved bearer-auth policy in the form the
// middleware consumes. Keeping the conversion here means the middleware
// stays unaware of ServerConfig and config.AuthConfig.
func (c ServerConfig) authPolicy() authPolicy {
	return authPolicy{
		Token:                                    c.Auth.Token,
		TrustPrivateNetwork:                      c.Auth.TrustPrivateNetwork,
		AllowUnauthenticatedPrivateNetworkWrites: c.Auth.AllowUnauthenticatedPrivateNetworkWrites,
		InsecureReadonly:                         c.InsecureReadonly,
		RequireTokenIdentity:                     c.Auth.RequireTokenIdentity,
	}
}

func (c ServerConfig) federationCredentialStore() config.FederationCredentialStore {
	if c.FederationCredentials != nil {
		return c.FederationCredentials
	}
	return config.DefaultFederationCredentialStore()
}

// Publish returns the event publisher for this config. It is derived rather
// than stored so it can never disagree with the Broadcaster and Hooks fields
// the SSE handler and tests wire directly.
func (c ServerConfig) Publish() EventPublisher {
	return NewEventPublisher(c.Broadcaster, c.Hooks)
}

// CloseThrottlePolicy is the runtime form of [close.throttle] in
// <KATA_HOME>/config.toml.
type CloseThrottlePolicy struct {
	SiblingBurstEnabled bool
	SiblingBurstWindow  time.Duration
}

// GitHubSyncRunner runs one durable GitHub sync binding.
type GitHubSyncRunner interface {
	RunOnce(context.Context, int64) (githubsync.RunResult, error)
}

// GitHubSyncRunnerConfig is the daemon-side configuration passed to a runner
// factory. Tests use this seam to replace the runner without replacing DB state.
type GitHubSyncRunnerConfig struct {
	Store     db.Storage
	Fetcher   githubsync.Fetcher
	EventSink func(context.Context, int64, []db.Event) error
	Logger    *slog.Logger
}

// GitHubSyncRunnerFactory creates a runner for one daemon-side sync request.
type GitHubSyncRunnerFactory func(GitHubSyncRunnerConfig) GitHubSyncRunner

// NewDefaultGitHubSyncRunner adapts the public daemon seam to githubsync.Runner.
func NewDefaultGitHubSyncRunner(cfg GitHubSyncRunnerConfig) GitHubSyncRunner {
	return githubsync.NewRunner(githubsync.RunnerConfig{
		Store:     cfg.Store,
		Fetcher:   cfg.Fetcher,
		EventSink: cfg.EventSink,
		Logger:    cfg.Logger,
	})
}

// Server bundles the http handler and lifecycle.
type Server struct {
	cfg         ServerConfig
	baseHandler http.Handler
	handler     http.Handler
	api         huma.API
	authPolicy  authPolicy

	shutdownTimeout time.Duration
}

// ErrHTTPHandlersUnjoined means graceful HTTP shutdown exhausted its deadline
// while one or more handlers could still be using daemon dependencies.
var ErrHTTPHandlersUnjoined = errors.New("server: HTTP handlers did not drain")

// ListenerBinding associates an owned listener with its request policy.
type ListenerBinding struct {
	Listener net.Listener
	Policy   ListenerPolicy
}

// NewServer wires routes onto a fresh http.ServeMux. The returned handler is
// safe to mount in tests via httptest.NewServer.
func NewServer(cfg ServerConfig) *Server {
	if cfg.Broadcaster == nil {
		cfg.Broadcaster = NewEventBroadcaster()
	}
	if cfg.Hooks == nil {
		cfg.Hooks = hooks.NewNoop()
	}
	if cfg.FederationCredentials == nil {
		cfg.FederationCredentials = config.DefaultFederationCredentialStore()
	}
	cfg.FederationCatalog = append([]config.CatalogDaemonConfig(nil), cfg.FederationCatalog...)
	cfg.WebDaemons = append([]config.CatalogDaemonConfig(nil), cfg.WebDaemons...)
	if cfg.UIStore == nil {
		cfg.UIStore, _ = cfg.DB.(db.UIStore)
	}

	mux := http.NewServeMux()
	humaConfig := huma.DefaultConfig("kata", APISchemaVersion)
	humaConfig.Components.Schemas = newAPISchemaRegistry()
	jsonFormat := huma.Format{
		Marshal: func(w io.Writer, value any) error {
			return jsonv2.MarshalWrite(w, value)
		},
		Unmarshal: func(data []byte, value any) error {
			return unmarshalAPIJSON(data, value)
		},
	}
	humaConfig.Formats = map[string]huma.Format{
		"application/json": jsonFormat,
		"json":             jsonFormat,
	}
	humaConfig.OpenAPIPath = "" // Plan 1: no /openapi.json served at runtime; see `kata openapi` + OpenAPIDocument
	humaConfig.DocsPath = ""
	humaConfig.Transformers = append(humaConfig.Transformers, api.TransformHumaError)
	// Drop DefaultConfig's SchemaLinkTransformer: it rebuilds response structs
	// via reflection (adding a $schema field), which silently bypasses any
	// MarshalJSON. Our APIError relies on MarshalJSON to emit the wire-spec
	// envelope shape, so we must disable the transform.
	humaConfig.CreateHooks = nil
	humaAPI := huma.NewAPI(humaConfig, api.WrapErrorAdapter(humago.NewAdapter(mux, "")))
	withEmbeddingProfile(humaAPI, cfg.EmbeddingProfile)
	withHostAccess(humaAPI, cfg.HostAccess)
	withExternalRootAdministration(humaAPI, cfg.Auth.AllowIdentityConnectorAdministration)

	s := &Server{cfg: cfg, api: humaAPI}
	registerRoutes(humaAPI, mux, cfg)
	if cfg.WebSessions != nil {
		registerUISessionHandlers(mux, cfg.WebSessions)
	}
	registerWebDaemonHandlers(mux, cfg)
	registerOpenAPIYAML(mux, cfg.HostAccess)
	registerPprofHandlers(mux)
	webHandler, err := kataweb.NewEmbeddedHandler()
	if err != nil {
		panic(fmt.Errorf("build embedded web handler: %w", err))
	}
	mux.Handle("/", webHandler)
	applyErrorEnvelopeResponses(humaAPI.OpenAPI())
	policy := cfg.authPolicy()
	policy.SelfAuthenticatedRoutes = newSelfAuthenticatedRouteMatcher(
		selfAuthenticatedRoutes(humaAPI.OpenAPI()))
	s.authPolicy = policy

	s.baseHandler = mux
	s.handler, _ = s.HandlerFor(ListenerPolicy{Kind: ListenerSocket})
	return s
}

// withGzip compresses responses with gzip when the client sends
// Accept-Encoding: gzip. Eligibility is limited to the JSON API surface and
// the OpenAPI YAML document; text/event-stream is deliberately excluded so
// SSE frames flush through to the client unbuffered (the events handler
// also depends on the ResponseWriter passing http.Flusher through, which
// gzhttp's wrapper preserves). Responses under gzhttp's default minimum
// size are sent as-is since gzip overhead would outweigh the savings.
func withGzip(next http.Handler) http.Handler {
	wrap, err := gzhttp.NewWrapper(gzhttp.ContentTypes([]string{
		"application/json",
		"application/openapi+yaml",
	}))
	if err != nil {
		// The options above are static; NewWrapper only fails on invalid
		// option values, so any error here is a programming error.
		panic(fmt.Errorf("build gzip middleware: %w", err))
	}
	return wrap(next)
}

// Handler returns the http.Handler suitable for httptest.NewServer.
func (s *Server) Handler() http.Handler { return s.handler }

// HandlerFor returns the shared route stack wrapped for one listener.
func (s *Server) HandlerFor(policy ListenerPolicy) (http.Handler, error) {
	base := s.baseHandler
	if base == nil {
		base = s.handler
	} else {
		base = withIdleAdmission(s.cfg.IdleAdmission, base)
		base = withFederationIngestPreauthorization(s.cfg, base)
		base = withTrustedProxyActor(s.cfg)(base)
		base = requireBearer(s.authPolicy, s.cfg.DB)(base)
		base = withGzip(base)
		base = withOwnerLocalTransport(base)
	}
	if policy.RequireBrowserSession {
		if s.cfg.WebSessions == nil {
			return nil, errors.New("browser listener policy requires a web session manager")
		}
		base = requireBrowserSession(s.cfg.WebSessions, policy, base)
	}
	handler, err := ApplyListenerPolicy(base, policy)
	if err != nil {
		return nil, err
	}
	return handler, nil
}

// API returns the underlying huma.API for handler registration in tests.
func (s *Server) API() huma.API { return s.api }

// Close releases server-owned resources. Currently a no-op since the DB is
// owned by the caller.
func (s *Server) Close() error { return nil }

func registerOpenAPIYAML(mux *http.ServeMux, hostAccess HostAccessController) {
	mux.HandleFunc(http.MethodGet+" /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeHostHTTP(w, r, hostAccess, HostOperation{
			ID: "openAPI", Method: http.MethodGet, Path: "/openapi.yaml",
			PathParams: map[string]string{},
		}) {
			return
		}
		out, err := OpenAPIYAML()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/openapi+yaml")
		_, _ = w.Write(out)
	})
}

func registerPprofHandlers(mux *http.ServeMux) {
	mux.HandleFunc(http.MethodGet+" /debug/pprof/", pprof.Index)
	mux.HandleFunc(http.MethodGet+" /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc(http.MethodGet+" /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc(http.MethodGet+" /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc(http.MethodGet+" /debug/pprof/trace", pprof.Trace)
}

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
	return s.serve(ctx, l, s.handler, nil)
}

func (s *Server) serve(
	ctx context.Context,
	l net.Listener,
	handler http.Handler,
	onExit func(error),
) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// BaseContext roots every request in the daemon ctx so long-lived
		// SSE handlers exit on Shutdown via r.Context().Done().
		BaseContext: func(net.Listener) context.Context { return serveCtx },
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-serveCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(
			context.WithoutCancel(serveCtx), s.httpShutdownTimeout(),
		)
		defer shutdownCancel()
		shutdownDone <- httpSrv.Shutdown(shutdownCtx)
	}()
	serveErr := httpSrv.Serve(l)
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if onExit != nil {
		onExit(serveErr)
	}
	cancel()
	shutdownErr := <-shutdownDone
	// A sibling listener or the lifecycle coordinator may close this listener
	// while Shutdown is doing the same work. The listener is already quiesced;
	// net.ErrClosed does not mean handlers failed to drain.
	if errors.Is(shutdownErr, net.ErrClosed) {
		shutdownErr = nil
	}
	if shutdownErr != nil {
		closeErr := httpSrv.Close()
		return errors.Join(
			serveErr,
			fmt.Errorf("%w: %v", ErrHTTPHandlersUnjoined, shutdownErr),
			closeErr,
		)
	}
	return serveErr
}

func (s *Server) httpShutdownTimeout() time.Duration {
	if s.shutdownTimeout > 0 {
		return s.shutdownTimeout
	}
	return 10 * time.Second
}

// ServeListeners serves all listener bindings under one lifecycle. A failure
// on either listener cancels and closes every sibling listener.
func (s *Server) ServeListeners(ctx context.Context, bindings ...ListenerBinding) error {
	return s.ServeListenersWithStop(ctx, nil, bindings...)
}

// ServeListenersWithStop is ServeListeners with an immediate notification
// when any listener exits. The callback may cancel the daemon root while this
// method continues joining HTTP handlers.
func (s *Server) ServeListenersWithStop(
	ctx context.Context,
	onStopping func(),
	bindings ...ListenerBinding,
) error {
	return s.ServeListenersWithLifecycle(ctx, nil, onStopping, bindings...)
}

// ServeListenersWithLifecycle is ServeListenersWithStop with a readiness
// callback. Readiness is reported only after every listener binding has been
// validated and its handler has been prepared, immediately before serving.
func (s *Server) ServeListenersWithLifecycle(
	ctx context.Context,
	onReady func() error,
	onStopping func(),
	bindings ...ListenerBinding,
) error {
	if len(bindings) == 0 {
		return errors.New("server: at least one listener is required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	handlers := make([]http.Handler, len(bindings))
	for i, binding := range bindings {
		if binding.Listener == nil {
			return errors.New("server: listener is required")
		}
		handler, err := s.HandlerFor(binding.Policy)
		if err != nil {
			return err
		}
		handlers[i] = handler
	}
	if onReady != nil {
		if err := onReady(); err != nil {
			return err
		}
	}
	var stopOnce sync.Once
	var firstCause error
	stopAll := func(cause error) {
		stopOnce.Do(func() {
			firstCause = cause
			if onStopping != nil {
				onStopping()
			}
			cancel()
			for _, binding := range bindings {
				_ = binding.Listener.Close()
			}
		})
	}
	results := make(chan error, len(bindings))
	for i, binding := range bindings {
		handler := handlers[i]
		go func(binding ListenerBinding, handler http.Handler) {
			results <- s.serve(runCtx, binding.Listener, handler, stopAll)
		}(binding, handler)
	}

	var serveErrs []error
	for range len(bindings) {
		if err := <-results; err != nil {
			serveErrs = append(serveErrs, err)
		}
	}
	serveErr := errors.Join(serveErrs...)
	if errors.Is(serveErr, ErrHTTPHandlersUnjoined) || firstCause != nil {
		return serveErr
	}
	if ctx.Err() != nil {
		return nil
	}
	return serveErr
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
		requireJSONMutation(next).ServeHTTP(w, r)
	})
}

func requireJSONMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
// receives mux so it can preserve the SSE endpoint's method-not-allowed
// contract around the Huma streaming route.
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
	registerIssueSyncHandlers(humaAPI, cfg)
	registerExternalRootHandlers(humaAPI, cfg)
	registerClaimHandlers(humaAPI, cfg)
	registerDigestHandlers(humaAPI, cfg)
	registerAuditHandlers(humaAPI, cfg)
	registerUIHandlers(humaAPI, cfg)
	registerUILaunchHandler(humaAPI, cfg)
}

// registerHealth registers /api/v1/ping and /api/v1/health.
func registerHealth(humaAPI huma.API, cfg ServerConfig) {
	registerHealthHandlers(humaAPI, cfg)
}

// registerProjects registers project-scoped routes (resolve, init, list, show).
func registerProjects(humaAPI huma.API, cfg ServerConfig) {
	registerProjectsHandlers(humaAPI, cfg)
	registerAuthorRewriteHandlers(humaAPI, cfg)
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

// validateActor returns a 400 validation error when actor is empty. Huma's
// `required:"true"` only checks presence, so actor validation is enforced
// again after request authentication resolves the authoritative actor.
func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return api.NewError(400, "validation", "actor is required", "", nil)
	}
	return nil
}

// Package kata exposes Kata as a listener-free application service.
//
// A Service owns its configured storage handle and core background workers.
// Callers mount Handler in their own HTTP server, run background work with Run,
// and release owned resources with Close. The package does not open a network
// listener, install signal handlers, or manage its caller's process lifecycle.
package kata

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/githubsync"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

// PostgresSchemaMode controls how a PostgreSQL-backed service treats its
// configured schema at startup.
type PostgresSchemaMode string

const (
	// PostgresSchemaBootstrap installs missing Kata migrations before serving.
	PostgresSchemaBootstrap PostgresSchemaMode = "bootstrap"
	// PostgresSchemaValidate requires an already-installed, compatible schema
	// and performs no schema installation.
	PostgresSchemaValidate PostgresSchemaMode = "validate"
)

// EmbeddingProfile selects which HTTP administration surfaces a mounted
// service exposes. The zero value preserves standalone-compatible operations
// supported by the public embedding configuration. Federation endpoint rebind
// remains daemon-only because mounted services do not expose a daemon catalog
// or exact credential-replacement contract.
type EmbeddingProfile string

// Embedding profiles supported by Config.Profile.
const (
	EmbeddingProfileStandalone EmbeddingProfile = ""
	// EmbeddingProfileRestricted is for applications that own project,
	// credential, federation, and external-integration administration. It keeps
	// task and federation transport behavior while failing closed on native
	// administration routes.
	EmbeddingProfileRestricted EmbeddingProfile = "restricted"
)

// PostgresConfig selects an isolated PostgreSQL schema and startup policy.
// Empty fields use Kata's standalone defaults.
type PostgresConfig struct {
	Schema                 string
	SchemaMode             PostgresSchemaMode
	SchemaOwner            string
	AllowInsecureTransport bool
}

// AuthConfig defines Kata-owned authentication for a mounted Service. Select
// exactly one of Token, TrustCallerAuthentication, or Config.Access.
type AuthConfig struct {
	// Token protects Kata's ordinary HTTP surface. Federation transport and
	// claim actions retain their own scoped credential checks.
	Token string
	// TrustCallerAuthentication disables Kata's bearer check because the
	// caller guarantees that Handler is mounted behind its own authentication
	// boundary. Never mount that handler directly on an untrusted listener.
	TrustCallerAuthentication bool
}

// GitHubSyncConfig supplies credentials used by GitHub issue synchronization.
// TokenEnv names an environment variable containing a token; token values are
// never stored in Config. Empty fields use Kata's standalone defaults.
type GitHubSyncConfig struct {
	TokenEnv  string
	TokenHost string
	Apps      []GitHubAppConfig
}

// PlaneSyncConfig selects the daemon-owned Plane origins and token selector.
// The API key is resolved from the environment and never stored in bindings.
type PlaneSyncConfig struct{ APIOrigin, WebOrigin, TokenEnv string }

// LinearSyncConfig selects a daemon-owned credential and its authorization type.
type LinearSyncConfig struct{ TokenEnv, AuthType string }

// TwentySyncConfig selects the daemon-owned Twenty origins and token selector.
// The API key is resolved from the environment and never stored in bindings.
type TwentySyncConfig struct{ APIOrigin, WebOrigin, TokenEnv string }

// TodoistSyncConfig selects the daemon-owned API token environment variable.
type TodoistSyncConfig struct{ APIOrigin, TokenEnv string }

// TickTickSyncConfig selects the daemon-owned TickTick token environment variable.
// Credentials are never accepted from binding config.
type TickTickSyncConfig struct{ TokenEnv string }

// NotionSyncConfig selects the daemon-owned Notion token environment variable.
// Empty TokenEnv uses KATA_NOTION_TOKEN; credentials are resolved only for runs.
type NotionSyncConfig struct {
	TokenEnv string
}

// GitHubAppConfig identifies one GitHub App installation credential.
type GitHubAppConfig struct {
	Host           string
	Owner          string
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
}

// Config contains the process-neutral inputs needed to construct a Service.
type Config struct {
	// DSN accepts a SQLite path, sqlite:// URL, or PostgreSQL URL.
	DSN          string
	Postgres     PostgresConfig
	Auth         AuthConfig
	GitHubSync   GitHubSyncConfig
	NotionSync   NotionSyncConfig
	PlaneSync    PlaneSyncConfig
	LinearSync   LinearSyncConfig
	TwentySync   TwentySyncConfig
	TodoistSync  TodoistSyncConfig
	TickTickSync TickTickSyncConfig
	// WebHandler optionally serves public, data-free browser assets alongside
	// the API. Non-API paths bypass Kata's bearer check. Nil keeps the service
	// API-only. Import go.kenn.io/kata/webui to opt into the bundled application.
	WebHandler http.Handler
	// DefaultTimezone applies to civil schedules without issue timezone.
	// Empty means UTC.
	DefaultTimezone string
	// Access selects host-supplied in-process authentication and authorization.
	// It is mutually exclusive with Auth.
	Access AccessController
	// FederationAccess optionally adds host-owned authorization after Kata has
	// authenticated a project-scoped federation credential. It does not replace
	// Kata's credential authentication.
	FederationAccess FederationAccessController
	// WorkerTransactionFence revalidates host authority from inside every
	// background-worker writable storage transaction. It is required when
	// Access is set.
	WorkerTransactionFence TransactionFence
	// Profile optionally removes native administration routes that an embedding
	// host owns. The zero value keeps the complete standalone-compatible API.
	Profile EmbeddingProfile
	// FederationCredentials isolates secret material from other Service
	// instances. Nil selects a service-owned in-memory store.
	FederationCredentials FederationCredentialStore
	StartedAt             time.Time
	Logger                *slog.Logger
}

type serviceDeps struct {
	notionSyncFetcher          notionsync.Fetcher
	notionSyncFetcherFactory   func(config.NotionSyncConfig) notionsync.Fetcher
	planeSyncFetcher           planesync.Fetcher
	twentySyncFetcher          twentysync.Fetcher
	planeSyncFetcherFactory    func(config.PlaneSyncConfig) planesync.Fetcher
	linearSyncFetcher          linearsync.Fetcher
	linearSyncFetcherFactory   func(config.LinearSyncConfig) linearsync.Fetcher
	twentySyncFetcherFactory   func(config.TwentySyncConfig) twentysync.Fetcher
	gitHubSyncFetcher          githubsync.Fetcher
	gitHubSyncFetcherFactory   func(config.GitHubSyncConfig) githubsync.Fetcher
	todoistSyncFetcher         todoistsync.Fetcher
	todoistSyncFetcherFactory  func(config.TodoistSyncConfig) todoistsync.Fetcher
	tickTickSyncFetcher        tickticksync.Fetcher
	tickTickSyncFetcherFactory func(config.TickTickSyncConfig) tickticksync.Fetcher
}

// Service is a mountable Kata HTTP application and its owned lifecycle.
type Service struct {
	store                  db.Storage
	server                 *daemon.Server
	broadcaster            *daemon.EventBroadcaster
	publish                daemon.EventPublisher
	federationWake         chan struct{}
	gitHubSyncWake         chan struct{}
	gitHubSyncFetcher      githubsync.Fetcher
	gitHubSyncProgress     *githubsync.ProgressTracker
	notionSyncWake         chan struct{}
	notionSyncFetcher      notionsync.Fetcher
	notionSyncProgress     *issuesync.ProgressTracker
	tickTickSyncWake       chan struct{}
	tickTickSyncFetcher    tickticksync.Fetcher
	tickTickSyncProgress   *issuesync.ProgressTracker
	planeSyncWake          chan struct{}
	twentySyncWake         chan struct{}
	planeSyncFetcher       planesync.Fetcher
	twentySyncFetcher      twentysync.Fetcher
	planeSyncProgress      *issuesync.ProgressTracker
	linearSyncWake         chan struct{}
	linearSyncFetcher      linearsync.Fetcher
	linearSyncProgress     *issuesync.ProgressTracker
	twentySyncProgress     *issuesync.ProgressTracker
	todoistSyncWake        chan struct{}
	todoistSyncFetcher     todoistsync.Fetcher
	todoistSyncProgress    *issuesync.ProgressTracker
	federationCredentials  config.FederationCredentialStore
	logger                 *slog.Logger
	defaultTimezone        string
	hostAccessEnabled      bool
	workerTransactionFence TransactionFence
	lifetimeCtx            context.Context
	lifetimeCancel         context.CancelFunc
	handlerWG              sync.WaitGroup

	mu        sync.Mutex
	running   bool
	runCancel context.CancelFunc
	runDone   chan struct{}
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

// New opens the configured backend and constructs a listener-free service.
// The returned Service owns the backend handle and closes it from Close.
func New(ctx context.Context, cfg Config) (*Service, error) {
	return newService(ctx, cfg, serviceDeps{})
}

func newService(ctx context.Context, cfg Config, deps serviceDeps) (*Service, error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("kata: storage DSN is required")
	}
	if cfg.Profile != EmbeddingProfileStandalone && cfg.Profile != EmbeddingProfileRestricted {
		return nil, fmt.Errorf("kata: unknown embedding profile %q", cfg.Profile)
	}
	cfg.DefaultTimezone = strings.TrimSpace(cfg.DefaultTimezone)
	if cfg.DefaultTimezone != "" {
		if _, err := time.LoadLocation(cfg.DefaultTimezone); err != nil {
			return nil, fmt.Errorf("kata: default timezone %q is not a valid IANA timezone: %w", cfg.DefaultTimezone, err)
		}
	}
	usesAccess := cfg.Access != nil
	usesToken := strings.TrimSpace(cfg.Auth.Token) != ""
	usesTrustedCaller := cfg.Auth.TrustCallerAuthentication
	if !usesAccess && !usesToken && !usesTrustedCaller {
		return nil, errors.New("kata: auth token is required unless caller authentication is explicitly trusted")
	}
	if boolCount(usesAccess, usesToken, usesTrustedCaller) != 1 {
		return nil, errors.New("kata: auth token, trusted caller authentication, and host access are mutually exclusive")
	}
	gitHubSyncConfig, err := resolveGitHubSyncConfig(cfg.GitHubSync)
	if err != nil {
		return nil, fmt.Errorf("kata: GitHub sync config: %w", err)
	}
	notionSyncConfig, err := config.NormalizeNotionSyncConfig(config.NotionSyncConfig{TokenEnv: cfg.NotionSync.TokenEnv})
	if err != nil {
		return nil, fmt.Errorf("kata: Notion sync config: %w", err)
	}
	planeSyncConfig, err := config.NormalizePlaneSyncConfig(config.PlaneSyncConfig{APIOrigin: cfg.PlaneSync.APIOrigin, WebOrigin: cfg.PlaneSync.WebOrigin, TokenEnv: cfg.PlaneSync.TokenEnv})
	if err != nil {
		return nil, fmt.Errorf("kata: Plane sync config: %w", err)
	}
	linearSyncConfig, err := config.NormalizeLinearSyncConfig(config.LinearSyncConfig{TokenEnv: cfg.LinearSync.TokenEnv, AuthType: cfg.LinearSync.AuthType})
	if err != nil {
		return nil, fmt.Errorf("kata: Linear sync config: %w", err)
	}
	twentySyncConfig, err := config.NormalizeTwentySyncConfig(config.TwentySyncConfig{APIOrigin: cfg.TwentySync.APIOrigin, WebOrigin: cfg.TwentySync.WebOrigin, TokenEnv: cfg.TwentySync.TokenEnv})
	if err != nil {
		return nil, fmt.Errorf("kata: Twenty sync config: %w", err)
	}
	todoistSyncConfig, err := config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{APIOrigin: cfg.TodoistSync.APIOrigin, TokenEnv: cfg.TodoistSync.TokenEnv})
	if err != nil {
		return nil, fmt.Errorf("kata: Todoist sync config: %w", err)
	}
	tickTickSyncConfig, err := config.NormalizeTickTickSyncConfig(config.TickTickSyncConfig{TokenEnv: cfg.TickTickSync.TokenEnv})
	if err != nil {
		return nil, fmt.Errorf("kata: TickTick sync config: %w", err)
	}
	publicFederationCredentials := cfg.FederationCredentials
	if publicFederationCredentials == nil {
		publicFederationCredentials = newMemoryFederationCredentialStore()
	}
	federationCredentials := serviceCredentialStoreAdapter{store: publicFederationCredentials}

	openCfg := storeopen.DefaultConfig()
	pgCfg := cfg.Postgres
	openCfg.Postgres = pgstore.ConfigFromValues(
		pgCfg.Schema,
		string(pgCfg.SchemaMode),
		pgCfg.SchemaOwner,
		pgCfg.AllowInsecureTransport,
	)
	store, err := storeopen.OpenWithConfig(ctx, cfg.DSN, openCfg, db.Serving())
	if err != nil {
		return nil, fmt.Errorf("kata: open storage: %w", err)
	}

	startedAt := cfg.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	lifetimeCtx, lifetimeCancel := context.WithCancel(context.Background())
	broadcaster := daemon.NewEventBroadcaster()
	hookSink := hooks.NewNoop()
	publisher := daemon.NewEventPublisher(broadcaster, hookSink)
	federationWake := make(chan struct{}, 1)
	wakeFederation := func() {
		select {
		case federationWake <- struct{}{}:
		default:
		}
	}
	gitHubSyncWake := make(chan struct{}, 1)
	wakeGitHubSync := func() {
		select {
		case gitHubSyncWake <- struct{}{}:
		default:
		}
	}
	gitHubSyncFetcher := deps.gitHubSyncFetcher
	if gitHubSyncFetcher == nil {
		factory := deps.gitHubSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.GitHubSyncConfig) githubsync.Fetcher {
				return githubsync.NewHTTPFetcher(githubsync.HTTPFetcherConfig{
					CredentialResolver: githubsync.NewCredentialResolver(cfg, nil),
				})
			}
		}
		gitHubSyncFetcher = factory(gitHubSyncConfig)
	}
	gitHubSyncProgress := githubsync.NewProgressTracker()
	notionSyncWake := make(chan struct{}, 1)
	wakeNotionSync := func() { signalWake(notionSyncWake) }
	notionSyncFetcher := deps.notionSyncFetcher
	if notionSyncFetcher == nil {
		factory := deps.notionSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.NotionSyncConfig) notionsync.Fetcher {
				return notionsync.NewClient(notionsync.ClientConfig{TokenEnv: cfg.TokenEnv})
			}
		}
		notionSyncFetcher = factory(notionSyncConfig)
	}
	notionSyncProgress := issuesync.NewProgressTracker()
	planeSyncWake := make(chan struct{}, 1)
	wakePlaneSync := func() { signalWake(planeSyncWake) }
	planeSyncFetcher := deps.planeSyncFetcher
	if planeSyncFetcher == nil {
		factory := deps.planeSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.PlaneSyncConfig) planesync.Fetcher {
				return planesync.NewClient(planesync.ClientConfig{Daemon: cfg})
			}
		}
		planeSyncFetcher = factory(planeSyncConfig)
	}
	planeSyncProgress := issuesync.NewProgressTracker()
	linearSyncWake := make(chan struct{}, 1)
	wakeLinearSync := func() { signalWake(linearSyncWake) }
	linearSyncFetcher := deps.linearSyncFetcher
	if linearSyncFetcher == nil {
		factory := deps.linearSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.LinearSyncConfig) linearsync.Fetcher {
				return linearsync.NewClient(linearsync.ClientConfig{Daemon: cfg})
			}
		}
		linearSyncFetcher = factory(linearSyncConfig)
	}
	linearSyncProgress := issuesync.NewProgressTracker()
	twentySyncWake := make(chan struct{}, 1)
	wakeTwentySync := func() { signalWake(twentySyncWake) }
	twentySyncFetcher := deps.twentySyncFetcher
	if twentySyncFetcher == nil {
		factory := deps.twentySyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.TwentySyncConfig) twentysync.Fetcher {
				return twentysync.NewClient(twentysync.ClientConfig{Daemon: cfg})
			}
		}
		twentySyncFetcher = factory(twentySyncConfig)
	}
	twentySyncProgress := issuesync.NewProgressTracker()
	todoistSyncWake := make(chan struct{}, 1)
	wakeTodoistSync := func() { signalWake(todoistSyncWake) }
	todoistSyncFetcher := deps.todoistSyncFetcher
	if todoistSyncFetcher == nil {
		factory := deps.todoistSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.TodoistSyncConfig) todoistsync.Fetcher {
				return todoistsync.NewClient(todoistsync.ClientConfig{Daemon: cfg})
			}
		}
		todoistSyncFetcher = factory(todoistSyncConfig)
	}
	todoistSyncProgress := issuesync.NewProgressTracker()
	tickTickSyncWake := make(chan struct{}, 1)
	wakeTickTickSync := func() { signalWake(tickTickSyncWake) }
	tickTickSyncFetcher := deps.tickTickSyncFetcher
	if tickTickSyncFetcher == nil {
		factory := deps.tickTickSyncFetcherFactory
		if factory == nil {
			factory = func(cfg config.TickTickSyncConfig) tickticksync.Fetcher {
				return tickticksync.NewClient(tickticksync.ClientConfig{TokenEnv: cfg.TokenEnv})
			}
		}
		tickTickSyncFetcher = factory(tickTickSyncConfig)
	}
	tickTickSyncProgress := issuesync.NewProgressTracker()
	var hostAccess daemon.HostAccessController
	if cfg.Access != nil {
		hostAccess = hostAccessControllerAdapter{controller: cfg.Access}
	}
	var hostFederationAccess daemon.HostFederationAccessController
	if cfg.FederationAccess != nil {
		hostFederationAccess = hostFederationAccessControllerAdapter{
			controller: cfg.FederationAccess,
		}
	}
	server := daemon.NewServer(daemon.ServerConfig{
		DB:                      store,
		WebHandler:              cfg.WebHandler,
		DefaultTimezone:         cfg.DefaultTimezone,
		StartedAt:               startedAt,
		Broadcaster:             broadcaster,
		FederationWake:          wakeFederation,
		FederationCredentials:   federationCredentials,
		DisableFederationRebind: true,
		GitHubSyncFetcher:       gitHubSyncFetcher,
		GitHubSyncProgress:      gitHubSyncProgress,
		GitHubSyncConfig:        gitHubSyncConfig,
		GitHubSyncWake:          wakeGitHubSync,
		NotionSyncFetcher:       notionSyncFetcher,
		NotionSyncProgress:      notionSyncProgress,
		NotionSyncConfig:        notionSyncConfig,
		NotionSyncWake:          wakeNotionSync,
		TickTickSyncFetcher:     tickTickSyncFetcher,
		TickTickSyncProgress:    tickTickSyncProgress,
		TickTickSyncConfig:      tickTickSyncConfig,
		TickTickSyncWake:        wakeTickTickSync,
		PlaneSyncFetcher:        planeSyncFetcher,
		TwentySyncFetcher:       twentySyncFetcher,
		PlaneSyncProgress:       planeSyncProgress,
		TwentySyncProgress:      twentySyncProgress,
		PlaneSyncConfig:         planeSyncConfig,
		TwentySyncConfig:        twentySyncConfig,
		PlaneSyncWake:           wakePlaneSync,
		LinearSyncFetcher:       linearSyncFetcher,
		LinearSyncProgress:      linearSyncProgress,
		LinearSyncConfig:        linearSyncConfig,
		LinearSyncWake:          wakeLinearSync,
		TwentySyncWake:          wakeTwentySync,
		TodoistSyncConfig:       todoistSyncConfig,
		TodoistSyncFetcher:      todoistSyncFetcher,
		TodoistSyncWake:         wakeTodoistSync,
		TodoistSyncProgress:     todoistSyncProgress,
		Hooks:                   hookSink,
		Auth:                    config.AuthConfig{Token: cfg.Auth.Token},
		HostAccess:              hostAccess,
		HostFederationAccess:    hostFederationAccess,
		EmbeddingProfile:        daemon.EmbeddingProfile(cfg.Profile),
		Logger:                  logger,
	})

	return &Service{
		store:                  store,
		server:                 server,
		broadcaster:            broadcaster,
		publish:                publisher,
		federationWake:         federationWake,
		gitHubSyncWake:         gitHubSyncWake,
		gitHubSyncFetcher:      gitHubSyncFetcher,
		gitHubSyncProgress:     gitHubSyncProgress,
		notionSyncWake:         notionSyncWake,
		notionSyncFetcher:      notionSyncFetcher,
		notionSyncProgress:     notionSyncProgress,
		tickTickSyncWake:       tickTickSyncWake,
		tickTickSyncFetcher:    tickTickSyncFetcher,
		tickTickSyncProgress:   tickTickSyncProgress,
		planeSyncWake:          planeSyncWake,
		twentySyncWake:         twentySyncWake,
		planeSyncFetcher:       planeSyncFetcher,
		twentySyncFetcher:      twentySyncFetcher,
		planeSyncProgress:      planeSyncProgress,
		linearSyncWake:         linearSyncWake,
		linearSyncFetcher:      linearSyncFetcher,
		linearSyncProgress:     linearSyncProgress,
		twentySyncProgress:     twentySyncProgress,
		todoistSyncWake:        todoistSyncWake,
		todoistSyncFetcher:     todoistSyncFetcher,
		todoistSyncProgress:    todoistSyncProgress,
		federationCredentials:  federationCredentials,
		logger:                 logger,
		defaultTimezone:        cfg.DefaultTimezone,
		hostAccessEnabled:      cfg.Access != nil,
		workerTransactionFence: cfg.WorkerTransactionFence,
		lifetimeCtx:            lifetimeCtx,
		lifetimeCancel:         lifetimeCancel,
		closeDone:              make(chan struct{}),
	}, nil
}

func resolveGitHubSyncConfig(cfg GitHubSyncConfig) (config.GitHubSyncConfig, error) {
	apps := make([]config.GitHubAppConfig, len(cfg.Apps))
	for i := range cfg.Apps {
		app := cfg.Apps[i]
		apps[i] = config.GitHubAppConfig{
			Host:           app.Host,
			Owner:          app.Owner,
			AppID:          app.AppID,
			InstallationID: app.InstallationID,
			PrivateKeyPath: app.PrivateKeyPath,
		}
	}
	return config.NormalizeGitHubSyncConfig(config.GitHubSyncConfig{
		TokenEnv: cfg.TokenEnv, TokenHost: cfg.TokenHost, Apps: apps,
	})
}

// Handler returns the HTTP application for mounting in a caller-owned server.
func (s *Service) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// HandlerAt mounts the complete Kata HTTP application below one clean,
// absolute path. The returned handler owns only that path. Requests for the
// path without its trailing slash are redirected so relative browser assets
// and API requests stay beneath the mount.
func (s *Service) HandlerAt(mountPath string) (http.Handler, error) {
	if mountPath == "" || mountPath == "/" || !strings.HasPrefix(mountPath, "/") ||
		strings.HasSuffix(mountPath, "/") || path.Clean(mountPath) != mountPath ||
		strings.Contains(mountPath, `\`) {
		return nil, errors.New("kata: mount path must be a clean absolute path without a trailing slash")
	}
	if mountPath == "/api" || strings.HasPrefix(mountPath, "/api/") {
		return nil, errors.New("kata: mount path must not overlap the API namespace")
	}
	for _, r := range mountPath {
		if r < 0x20 || r == 0x7f {
			return nil, errors.New("kata: mount path must not contain control characters")
		}
	}

	application := s.Handler()
	mounted := http.StripPrefix(mountPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			request := r.Clone(r.Context())
			request.URL.Path = "/kata"
			request.URL.RawPath = ""
			application.ServeHTTP(w, request)
			return
		}
		application.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mountPath {
			location := (&url.URL{
				Path: path.Base(mountPath) + "/", RawQuery: r.URL.RawQuery,
			}).String()
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, mountPath+"/") {
			http.NotFound(w, r)
			return
		}
		mounted.ServeHTTP(w, r)
	}), nil
}

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "kata service is closed", http.StatusServiceUnavailable)
		return
	}
	s.handlerWG.Add(1)
	lifetimeCtx := s.lifetimeCtx
	s.mu.Unlock()
	defer s.handlerWG.Done()

	requestCtx, cancel := context.WithCancel(r.Context())
	stopLifetimeCancel := context.AfterFunc(lifetimeCtx, cancel)
	defer func() {
		stopLifetimeCancel()
		cancel()
	}()
	if principal, ok := principalFromContext(requestCtx); s.hostAccessEnabled && ok {
		requestCtx = daemon.WithPrincipal(requestCtx, daemon.Principal{
			Kind: daemon.PrincipalHost, Subject: principal.Subject, Actor: principal.Actor,
		})
	}
	s.server.Handler().ServeHTTP(w, r.WithContext(requestCtx))
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

type hostAccessControllerAdapter struct {
	controller AccessController
}

type hostFederationAccessControllerAdapter struct {
	controller FederationAccessController
}

func (a hostFederationAccessControllerAdapter) AuthorizeFederation(
	ctx context.Context,
	request daemon.HostFederationAccessRequest,
) (daemon.HostFederationAccessDecision, error) {
	if a.controller == nil {
		return daemon.HostFederationAccessDecision{}, nil
	}
	decision, err := a.controller.AuthorizeFederation(ctx, FederationAccessRequest{
		Enrollment: publicFederationEnrollment(request.Enrollment, request.Project.UID),
		Project:    publicProject(request.Project),
		Capability: FederationCapability(request.Capability),
		Operation: FederationOperation{
			ID: request.Operation.ID, Mutation: request.Operation.Mutation,
		},
	})
	if errors.Is(err, ErrAccessDenied) {
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostAccessDenied
	}
	if errors.Is(err, ErrFederationAdmissionLimited) {
		return daemon.HostFederationAccessDecision{}, daemon.ErrHostFederationAdmissionLimited
	}
	if err != nil {
		return daemon.HostFederationAccessDecision{}, err
	}
	var transactionFence db.TransactionFence
	if decision.TransactionFence != nil {
		transactionFence = func(ctx context.Context, transaction db.Transaction) error {
			err := decision.TransactionFence(ctx, transaction)
			if errors.Is(err, ErrAccessDenied) {
				return errors.Join(daemon.ErrHostAccessDenied, err)
			}
			return err
		}
	}
	return daemon.HostFederationAccessDecision{TransactionFence: transactionFence}, nil
}

func (a hostAccessControllerAdapter) Authorize(
	ctx context.Context,
	request daemon.HostAccessRequest,
) (daemon.HostAccessDecision, error) {
	if a.controller == nil {
		return daemon.HostAccessDecision{}, nil
	}
	decision, err := a.controller.Authorize(ctx, AccessRequest{
		Principal: Principal{Subject: request.Subject, Actor: request.Actor},
		Operation: Operation{
			ID: request.Operation.ID, Method: request.Operation.Method, Path: request.Operation.Path,
			PathParams: request.Operation.PathParams,
			Policy: OperationPolicy{
				Kind:       OperationKind(request.Operation.Policy.Kind),
				Capability: Capability(request.Operation.Policy.Capability),
				Mutation:   request.Operation.Policy.Mutation,
				LongLived:  request.Operation.Policy.LongLived,
			},
			ProjectIDs:  append([]int64(nil), request.Operation.ProjectIDs...),
			ProjectUIDs: append([]string(nil), request.Operation.ProjectUIDs...),
			AllProjects: request.Operation.AllProjects,
		},
	})
	if errors.Is(err, ErrAccessDenied) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	if err != nil {
		return daemon.HostAccessDecision{}, err
	}
	var revalidate func(context.Context) error
	if decision.Lease != nil {
		revalidate = decision.Lease.Revalidate
	}
	var transactionFence db.TransactionFence
	if decision.TransactionFence != nil {
		transactionFence = func(ctx context.Context, transaction db.Transaction) error {
			err := decision.TransactionFence(ctx, transaction)
			if errors.Is(err, ErrAccessDenied) {
				return errors.Join(daemon.ErrHostAccessDenied, err)
			}
			return err
		}
	}
	return daemon.HostAccessDecision{
		Revalidate: revalidate, TransactionFence: transactionFence,
	}, nil
}

// Run executes Kata's federation, GitHub, Notion, Plane, and Linear synchronization, timed-claim,
// due-notification, and assignment-expiry workers until ctx is canceled or
// Close is called. Run does not start a listener and may be called only once
// at a time.
func (s *Service) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("kata: run context is required")
	}
	if s.hostAccessEnabled && s.workerTransactionFence == nil {
		return errors.New("kata: worker transaction fence is required with host access")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("kata: service is closed")
	}
	if s.running {
		s.mu.Unlock()
		return errors.New("kata: service is already running")
	}
	runCtx, cancel := context.WithCancel(ctx)
	fence := &fenceRecorder{cancel: cancel}
	if s.workerTransactionFence != nil {
		runCtx = db.WithTransactionFence(runCtx, func(
			fenceCtx context.Context,
			transaction db.Transaction,
		) error {
			return s.workerTransactionFence(fenceCtx, transaction)
		})
		runCtx = db.WithTransactionRollbackObserver(runCtx, func(err error) { _ = fence.record(err) })
	}
	done := make(chan struct{})
	s.running = true
	s.runCancel = cancel
	s.runDone = done
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.running = false
		s.runCancel = nil
		close(done)
		s.mu.Unlock()
	}()
	federationSub := s.broadcaster.Subscribe(daemon.SubFilter{})
	federationWakeDone := make(chan struct{})
	go func() {
		defer close(federationWakeDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case msg, ok := <-federationSub.Ch:
				if !ok {
					return
				}
				if msg.Kind == daemon.StreamKindEvent {
					signalWake(s.federationWake)
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-federationWakeDone
		federationSub.Unsub()
	}()

	runner := &federation.Runner{
		DB:          s.store,
		Credentials: s.federationCredentials,
		Interval:    30 * time.Second,
		Wake:        s.federationWake,
		OnError: func(err error) {
			s.logger.Error("kata federation worker", "err", err)
		},
		OnPulledEvents: func(projectID int64, events []db.Event) {
			s.publishWorkerEvents(projectID, events)
		},
	}
	gitHubSyncRunner := githubsync.NewRunner(githubsync.RunnerConfig{
		Progress: s.gitHubSyncProgress,
		Store:    s.store,
		Fetcher:  s.gitHubSyncFetcher,
		Logger:   s.logger,
		Interval: 5 * time.Minute,
		Wake:     s.gitHubSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	notionSyncRunner := notionsync.NewRunner(notionsync.RunnerConfig{
		Progress: s.notionSyncProgress,
		Store:    s.store,
		Fetcher:  s.notionSyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.notionSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	planeSyncRunner := planesync.NewRunner(planesync.RunnerConfig{
		Progress: s.planeSyncProgress,
		Store:    s.store,
		Fetcher:  s.planeSyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.planeSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	linearSyncRunner := linearsync.NewRunner(linearsync.RunnerConfig{
		Progress: s.linearSyncProgress,
		Store:    s.store,
		Fetcher:  s.linearSyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.linearSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	twentySyncRunner := twentysync.NewRunner(twentysync.RunnerConfig{
		Progress: s.twentySyncProgress,
		Store:    s.store,
		Fetcher:  s.twentySyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.twentySyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	todoistSyncRunner := todoistsync.NewRunner(todoistsync.RunnerConfig{
		Progress: s.todoistSyncProgress,
		Store:    s.store,
		Fetcher:  s.todoistSyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.todoistSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	tickTickSyncRunner := tickticksync.NewRunner(tickticksync.RunnerConfig{
		Progress: s.tickTickSyncProgress,
		Store:    s.store,
		Fetcher:  s.tickTickSyncFetcher,
		Logger:   s.logger,
		Interval: 30 * time.Second,
		Wake:     s.tickTickSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			s.publishWorkerEvents(projectID, events)
			return nil
		},
	})
	sweeper := daemon.NewTimedClaimSweeper(s.store, s.publish)
	sweeper.OnError = func(err error) {
		s.logger.Error("kata timed-claim worker", "err", err)
	}
	dueNotificationSweeper := daemon.NewDueNotificationSweeper(s.store, s.publish, s.defaultTimezone)
	dueNotificationSweeper.OnError = func(err error) {
		s.logger.Error("kata due-notification worker", "err", err)
	}
	assignmentSweeper := daemon.NewAssignmentSweeper(s.store, s.publish)
	assignmentSweeper.OnError = func(err error) {
		s.logger.Error("kata assignment-expiry worker", "err", err)
	}
	workers := []namedWorker{
		{name: "federation", run: runner.Run},
		{name: "github-sync", run: gitHubSyncRunner.Run},
		{name: "notion-sync", run: notionSyncRunner.Run},
		{name: "plane-sync", run: planeSyncRunner.Run},
		{name: "linear-sync", run: linearSyncRunner.Run},
		{name: "twenty-sync", run: twentySyncRunner.Run},
		{name: "todoist-sync", run: todoistSyncRunner.Run},
		{name: "ticktick-sync", run: tickTickSyncRunner.Run},
		{name: "timed-claim", run: sweeper.Run},
		{name: "due-notification", run: dueNotificationSweeper.Run},
		{name: "assignment-expiry", run: assignmentSweeper.Run},
	}
	workerErrs := make(chan error, len(workers))
	for _, worker := range workers {
		go func() {
			err := worker.run(runCtx)
			workerErrs <- worker.result(err, runCtx.Err())
		}()
	}

	var stop runStop
	workerResults := make([]error, 0, len(workers))
	select {
	case <-runCtx.Done():
		stop = runStop{reason: stopContextDone, err: runCtx.Err()}
	case err := <-workerErrs:
		stop = runStop{reason: stopWorkerExit, err: err}
		workerResults = append(workerResults, err)
	}
	// Sampled here, next to the stop it qualifies: a Close that lands later
	// must not retroactively change what an already-recorded error means.
	callerStop := ctx.Err()
	if callerStop == nil && s.isClosed() {
		callerStop = context.Canceled
	}
	fence.beginUnwind()
	for len(workerResults) < len(workers) {
		workerResults = append(workerResults, <-workerErrs)
	}
	// A fence rejection cancels the run, so the select above sees it as
	// stopContextDone. The recorder holds the real reason.
	if fenceErr := fence.first(); fenceErr != nil {
		stop = runStop{reason: stopFenceFailure, err: fenceErr}
	}
	return runResult(stop, workerResults, callerStop)
}

// publishWorkerEvents fans a background worker's events out to both event
// surfaces. Workers route through here so none can grow its own
// half-wired copy of the pairing again.
func (s *Service) publishWorkerEvents(projectID int64, events []db.Event) {
	s.publish.Events(projectID, events)
}

func signalWake(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// fenceRecorder keeps the first finalized worker-transaction-fence rejection and
// cancels the run after rollback and host cleanup so the remaining workers stop.
// Once Run begins its own unwind,
// cancellation-only fence results caused by that cancel are ignored; genuine
// failures still take priority over the stop that began the unwind.
//
// Recording replaces the old report channel plus its non-blocking re-drain:
// every worker's exit is received after rollback finalization has returned, so
// reading first() once the drain is complete cannot race a record.
type fenceRecorder struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	err       error
	unwinding bool
}

func (f *fenceRecorder) record(err error) error {
	if err == nil {
		return nil
	}
	cancellationOnly := isCancellationOnly(err)
	f.mu.Lock()
	if f.err == nil && (!f.unwinding || !cancellationOnly) {
		f.err = err
	}
	f.mu.Unlock()
	f.cancel()
	return err
}

// beginUnwind marks Run's internal cancellation boundary before firing the
// cancel that can cause in-flight fence callbacks to return context.Canceled.
func (f *fenceRecorder) beginUnwind() {
	f.mu.Lock()
	f.unwinding = true
	f.mu.Unlock()
	f.cancel()
}

func (f *fenceRecorder) first() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// namedWorker pairs a background worker's run function with the name Run uses
// to attribute its failure.
type namedWorker struct {
	name string
	run  func(context.Context) error
}

// result normalizes a worker's exit: the run context's terminal error is how a
// worker reports a clean stop, anything else is a failure attributed to it.
func (w namedWorker) result(err, terminal error) error {
	if err == nil || isOnlyError(err, terminal) {
		return nil
	}
	return fmt.Errorf("kata: %s worker: %w", w.name, err)
}

// isOnlyError reports whether every non-nil leaf in err's causal graph matches
// target. errors.Is alone also matches joins that contain a genuine failure.
func isOnlyError(err, target error) bool {
	if err == nil || target == nil {
		return false
	}
	switch err := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, cause := range err.Unwrap() {
			if cause == nil {
				continue
			}
			found = true
			if !isOnlyError(cause, target) {
				return false
			}
		}
		return found
	case interface{ Unwrap() error }:
		if cause := err.Unwrap(); cause != nil {
			return isOnlyError(cause, target)
		}
	}
	return errors.Is(err, target)
}

func isCancellationOnly(err error) bool {
	return isOnlyError(err, context.Canceled)
}

// stopReason is why Run began unwinding, recorded when it happens rather than
// inferred afterwards from state that has since moved on.
type stopReason int

const (
	stopContextDone stopReason = iota
	stopFenceFailure
	stopWorkerExit
)

// runStop is the recorded stop decision and the error that carried the news.
type runStop struct {
	reason stopReason
	err    error
}

// runResult turns the recorded stop into Run's return value. It is a function
// so the shutdown policy is stated once, in one place, instead of emerging
// from the order of four probes of state that has already moved on.
//
// Policy (design decision D1): genuine worker failures reach the caller even
// when shutdown was in flight. A fence error composed only of the caller's
// terminal error is clean; mixed failures still surface. workerResults have
// already been normalized by namedWorker.result.
func runResult(stop runStop, workerResults []error, callerStop error) error {
	switch stop.reason {
	case stopFenceFailure:
		if isOnlyError(stop.err, callerStop) {
			// The fence was rejected *by* the shutdown it is unwinding; the
			// workers still report their own outcomes below.
			break
		}
		return fmt.Errorf("kata: worker transaction fence: %w", stop.err)
	case stopContextDone, stopWorkerExit:
		// The caller learns what the workers reported.
	}
	return errors.Join(workerResults...)
}

func (s *Service) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close stops active background work and releases every resource owned by the
// service. It is safe to call Close more than once.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	}
	s.closed = true
	s.lifetimeCancel()
	cancel := s.runCancel
	runDone := s.runDone
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		<-runDone
	}
	s.handlerWG.Wait()
	err := errors.Join(s.server.Close(), s.store.Close())

	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

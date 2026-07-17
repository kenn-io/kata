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

// PostgresConfig selects an isolated PostgreSQL schema and startup policy.
// Empty fields use Kata's standalone defaults.
type PostgresConfig struct {
	Schema                 string
	SchemaMode             PostgresSchemaMode
	SchemaOwner            string
	AllowInsecureTransport bool
}

// AuthConfig defines the authentication boundary for a mounted Service.
// Exactly one of Token or TrustCallerAuthentication must be selected.
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
	DSN        string
	Postgres   PostgresConfig
	Auth       AuthConfig
	GitHubSync GitHubSyncConfig
	// FederationCredentials isolates secret material from other Service
	// instances. Nil selects a service-owned in-memory store.
	FederationCredentials FederationCredentialStore
	StartedAt             time.Time
	Logger                *slog.Logger
}

type serviceDeps struct {
	gitHubSyncFetcher        githubsync.Fetcher
	gitHubSyncFetcherFactory func(config.GitHubSyncConfig) githubsync.Fetcher
}

// Service is a mountable Kata HTTP application and its owned lifecycle.
type Service struct {
	store                 db.Storage
	server                *daemon.Server
	broadcaster           *daemon.EventBroadcaster
	hookSink              hooks.Sink
	federationWake        chan struct{}
	gitHubSyncWake        chan struct{}
	gitHubSyncFetcher     githubsync.Fetcher
	federationCredentials config.FederationCredentialStore
	logger                *slog.Logger
	lifetimeCtx           context.Context
	lifetimeCancel        context.CancelFunc
	handlerWG             sync.WaitGroup

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
	if strings.TrimSpace(cfg.Auth.Token) == "" && !cfg.Auth.TrustCallerAuthentication {
		return nil, errors.New("kata: auth token is required unless caller authentication is explicitly trusted")
	}
	if strings.TrimSpace(cfg.Auth.Token) != "" && cfg.Auth.TrustCallerAuthentication {
		return nil, errors.New("kata: auth token and trusted caller authentication are mutually exclusive")
	}
	gitHubSyncConfig, err := resolveGitHubSyncConfig(cfg.GitHubSync)
	if err != nil {
		return nil, fmt.Errorf("kata: GitHub sync config: %w", err)
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
	server := daemon.NewServer(daemon.ServerConfig{
		DB:                    store,
		StartedAt:             startedAt,
		Broadcaster:           broadcaster,
		FederationWake:        wakeFederation,
		FederationCredentials: federationCredentials,
		GitHubSyncFetcher:     gitHubSyncFetcher,
		GitHubSyncConfig:      gitHubSyncConfig,
		GitHubSyncWake:        wakeGitHubSync,
		Hooks:                 hookSink,
		Auth:                  config.AuthConfig{Token: cfg.Auth.Token},
		Logger:                logger,
	})

	return &Service{
		store:                 store,
		server:                server,
		broadcaster:           broadcaster,
		hookSink:              hookSink,
		federationWake:        federationWake,
		gitHubSyncWake:        gitHubSyncWake,
		gitHubSyncFetcher:     gitHubSyncFetcher,
		federationCredentials: federationCredentials,
		logger:                logger,
		lifetimeCtx:           lifetimeCtx,
		lifetimeCancel:        lifetimeCancel,
		closeDone:             make(chan struct{}),
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
	s.server.Handler().ServeHTTP(w, r.WithContext(requestCtx))
}

// Run executes Kata's federation, GitHub synchronization, and timed-claim
// workers until ctx is canceled or Close is called. Run does not start a
// listener and may be called only once at a time.
func (s *Service) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("kata: run context is required")
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
				if msg.Kind == "event" {
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

	workerErrs := make(chan error, 3)
	runner := &federation.Runner{
		DB:          s.store,
		Credentials: s.federationCredentials,
		Interval:    30 * time.Second,
		Wake:        s.federationWake,
		OnError: func(err error) {
			s.logger.Error("kata federation worker", "err", err)
		},
		OnPulledEvents: func(projectID int64, events []db.Event) {
			for i := range events {
				event := events[i]
				s.broadcaster.Broadcast(daemon.StreamMsg{
					Kind: "event", Event: &event, ProjectID: projectID,
				})
			}
		},
	}
	gitHubSyncRunner := githubsync.NewRunner(githubsync.RunnerConfig{
		Store:    s.store,
		Fetcher:  s.gitHubSyncFetcher,
		Logger:   s.logger,
		Interval: 5 * time.Minute,
		Wake:     s.gitHubSyncWake,
		EventSink: func(_ context.Context, projectID int64, events []db.Event) error {
			for i := range events {
				event := events[i]
				s.broadcaster.Broadcast(daemon.StreamMsg{
					Kind: "event", Event: &event, ProjectID: projectID,
				})
				s.hookSink.Enqueue(event)
			}
			return nil
		},
	})
	sweeper := daemon.NewTimedClaimSweeper(s.store, s.broadcaster, s.hookSink)
	sweeper.OnError = func(err error) {
		s.logger.Error("kata timed-claim worker", "err", err)
	}
	go func() { workerErrs <- runner.Run(runCtx) }()
	go func() { workerErrs <- gitHubSyncRunner.Run(runCtx) }()
	go func() { workerErrs <- sweeper.Run(runCtx) }()

	workerResults := make([]error, 0, 3)
	select {
	case <-runCtx.Done():
	case err := <-workerErrs:
		workerResults = append(workerResults, err)
	}
	cancel()
	for len(workerResults) < 3 {
		workerResults = append(workerResults, <-workerErrs)
	}
	if runCtx.Err() != nil && (ctx.Err() != nil || s.isClosed()) {
		return nil
	}
	for i := range workerResults {
		workerResults[i] = normalizeWorkerError(workerResults[i])
	}
	return errors.Join(workerResults...)
}

func signalWake(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func normalizeWorkerError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
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

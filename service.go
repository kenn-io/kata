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

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/federation"
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

// Config contains the process-neutral inputs needed to construct a Service.
type Config struct {
	// DSN accepts a SQLite path, sqlite:// URL, or PostgreSQL URL.
	DSN       string
	Postgres  PostgresConfig
	StartedAt time.Time
	Logger    *slog.Logger
}

// Service is a mountable Kata HTTP application and its owned lifecycle.
type Service struct {
	store       db.Storage
	server      *daemon.Server
	broadcaster *daemon.EventBroadcaster
	wake        chan struct{}
	logger      *slog.Logger

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
	if strings.TrimSpace(cfg.DSN) == "" {
		return nil, errors.New("kata: storage DSN is required")
	}

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
	broadcaster := daemon.NewEventBroadcaster()
	wake := make(chan struct{}, 1)
	wakeRunner := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	server := daemon.NewServer(daemon.ServerConfig{
		DB:             store,
		StartedAt:      startedAt,
		Broadcaster:    broadcaster,
		FederationWake: wakeRunner,
		Hooks:          hooks.NewNoop(),
		Logger:         logger,
	})

	return &Service{
		store:       store,
		server:      server,
		broadcaster: broadcaster,
		wake:        wake,
		logger:      logger,
		closeDone:   make(chan struct{}),
	}, nil
}

// Handler returns the HTTP application for mounting in a caller-owned server.
func (s *Service) Handler() http.Handler { return s.server.Handler() }

// Run executes Kata's core federation and timed-claim workers until ctx is
// canceled or Close is called. Run does not start a listener and may be called
// only once at a time.
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

	workerErrs := make(chan error, 2)
	sink := hooks.NewNoop()
	runner := &federation.Runner{
		DB:       s.store,
		Interval: 30 * time.Second,
		Wake:     s.wake,
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
	sweeper := daemon.NewTimedClaimSweeper(s.store, s.broadcaster, sink)
	sweeper.OnError = func(err error) {
		s.logger.Error("kata timed-claim worker", "err", err)
	}
	go func() { workerErrs <- runner.Run(runCtx) }()
	go func() { workerErrs <- sweeper.Run(runCtx) }()

	workerResults := make([]error, 0, 2)
	select {
	case <-runCtx.Done():
	case err := <-workerErrs:
		workerResults = append(workerResults, err)
	}
	cancel()
	for len(workerResults) < 2 {
		workerResults = append(workerResults, <-workerErrs)
	}
	if runCtx.Err() != nil && (ctx.Err() != nil || s.isClosed()) {
		return nil
	}
	return errors.Join(
		normalizeWorkerError(workerResults[0]),
		normalizeWorkerError(workerResults[1]),
	)
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
	cancel := s.runCancel
	runDone := s.runDone
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		<-runDone
	}
	err := errors.Join(s.server.Close(), s.store.Close())

	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

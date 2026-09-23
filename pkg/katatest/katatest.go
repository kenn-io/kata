// Package katatest runs a real, in-process Kata service for tests of code that
// talks to Kata through go.kenn.io/kata/pkg/client.
//
// Each Server owns a private SQLite database in a test temporary directory, so
// tests exercise the same HTTP API, validation, and error envelopes as a
// production daemon without a separate process or shared state.
package katatest

import (
	"context"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/kata"
	"go.kenn.io/kata/pkg/client"
)

// Server is a running Kata service reachable at Endpoint.
type Server struct {
	// Endpoint is the daemon base URL to hand to client constructors: an
	// http://127.0.0.1:<port> origin, or unix:///<path> with WithUnixSocket.
	Endpoint string
	// Service is the Kata service behind Endpoint.
	Service *kata.Service

	token string
}

type options struct {
	token string
	unix  bool
}

// Option configures New.
type Option func(*options)

// WithToken requires bearer token authentication for every API request.
// Without it, the service trusts callers the way an unauthenticated local
// daemon does.
func WithToken(token string) Option {
	return func(o *options) { o.token = token }
}

// WithUnixSocket serves the service on a Unix socket instead of loopback TCP.
func WithUnixSocket() Option {
	return func(o *options) { o.unix = true }
}

// New starts a Kata service and stops it when the test ends.
func New(tb testing.TB, opts ...Option) *Server {
	tb.Helper()
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	auth := kata.AuthConfig{TrustCallerAuthentication: true}
	if o.token != "" {
		auth = kata.AuthConfig{Token: o.token}
	}
	service, err := kata.New(tb.Context(), kata.Config{
		DSN:    filepath.Join(tb.TempDir(), "kata.db"),
		Auth:   auth,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		tb.Fatalf("katatest: start kata service: %v", err)
	}
	runCtx, stopRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runCtx) }()

	httpServer := httptest.NewUnstartedServer(service.Handler())
	endpoint := ""
	if o.unix {
		socket := unixSocketPath(tb)
		listener, err := net.Listen("unix", socket)
		if err != nil {
			tb.Fatalf("katatest: listen on %s: %v", socket, err)
		}
		_ = httpServer.Listener.Close()
		httpServer.Listener = listener
		endpoint = "unix://" + socket
	}
	httpServer.Start()
	if endpoint == "" {
		endpoint = httpServer.URL
	}
	tb.Cleanup(func() {
		httpServer.Close()
		stopRun()
		if err := <-runDone; err != nil {
			tb.Errorf("katatest: run kata service: %v", err)
		}
		if err := service.Close(); err != nil {
			tb.Errorf("katatest: close kata service: %v", err)
		}
	})
	return &Server{Endpoint: endpoint, Service: service, token: o.token}
}

// Client returns a client for the server, authenticated with the token from
// WithToken when one was set.
func (s *Server) Client(tb testing.TB, opts ...client.Option) *client.Client {
	tb.Helper()
	api, err := client.NewForTarget(tb.Context(), s.Endpoint, client.TargetAuth{Token: s.token}, opts...)
	if err != nil {
		tb.Fatalf("katatest: build client: %v", err)
	}
	return api
}

// unixSocketPath returns a socket path short enough for every platform's
// sun_path limit; test temporary directories can exceed it on macOS.
func unixSocketPath(tb testing.TB) string {
	tb.Helper()
	dir, err := os.MkdirTemp("", "katatest-")
	if err != nil {
		tb.Fatalf("katatest: create socket directory: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "kata.sock")
}

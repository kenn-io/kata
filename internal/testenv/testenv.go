// Package testenv provides a per-test harness that boots a real daemon over TCP
// loopback, suitable for integration tests.
package testenv

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Env is a per-test daemon + DB + HTTP client bundle.
type Env struct {
	URL         string
	HTTP        *http.Client
	DB          *sqlitestore.Store
	Home        string
	Broadcaster *daemon.EventBroadcaster
}

// Option configures the test daemon. Applied to a ServerConfig before
// daemon.NewServer; tests use options to opt into non-default policy
// (e.g. throttle disabled) without forking the constructor.
type Option func(*daemon.ServerConfig)

// WithCloseThrottleDisabled tells the test daemon to skip the close
// throttle / repeated-message guards, mirroring [close.throttle]
// enabled=false in <KATA_HOME>/config.toml.
func WithCloseThrottleDisabled() Option {
	return func(cfg *daemon.ServerConfig) {
		cfg.CloseThrottle.ThrottleDisabled = true
	}
}

// WithAuthToken tells the test daemon to enforce bearer-token auth with the
// given token. Tests using env.HTTP directly must set the Authorization
// header on every request except /ping and /health. Tests that drive the
// client package (or any helper built on top of it) get the header
// for free because the option also exports KATA_AUTH_TOKEN into the test
// process so client-side construction resolves the same token via
// client.NewHTTPClient.
func WithAuthToken(token string) Option {
	return func(cfg *daemon.ServerConfig) {
		cfg.Auth.Token = token
	}
}

// WithRequireTokenIdentity enables DB-backed bearer identity for tests.
func WithRequireTokenIdentity() Option {
	return func(cfg *daemon.ServerConfig) {
		cfg.Auth.RequireTokenIdentity = true
	}
}

// applyClientAuthEnv mirrors the WithAuthToken value into the
// KATA_AUTH_TOKEN env var inside the test process so any in-process
// client construction picks the token up automatically. Called by
// New / NewFromDB after options are folded into cfg.
func applyClientAuthEnv(t *testing.T, cfg daemon.ServerConfig) {
	t.Helper()
	if cfg.Auth.Token != "" {
		t.Setenv("KATA_AUTH_TOKEN", cfg.Auth.Token)
	}
}

// WithInsecureReadonly enables the dev escape hatch in the test daemon.
func WithInsecureReadonly() Option {
	return func(cfg *daemon.ServerConfig) {
		cfg.InsecureReadonly = true
	}
}

// New launches a daemon listening on a free loopback port. The DB lives under
// a temp KATA_HOME. Cleanup is wired via t.Cleanup.
func New(t *testing.T, opts ...Option) *Env {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DB", filepath.Join(home, "kata.db"))

	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate testenv db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	url, client, bcast := serveDaemon(t, d, opts...)
	return &Env{URL: url, HTTP: client, DB: d, Home: home, Broadcaster: bcast}
}

// NewFromDB launches a daemon backed by an existing SQLite database file. Use
// this when verifying the contents of a DB produced by import/restore/migration
// flows. KATA_HOME is not modified — the caller's environment is preserved.
func NewFromDB(t *testing.T, dbPath string) *Env {
	t.Helper()
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, dbPath)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate testenv db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	url, client, bcast := serveDaemon(t, d)
	return &Env{URL: url, HTTP: client, DB: d, Home: filepath.Dir(dbPath), Broadcaster: bcast}
}

// Get issues GET env.URL+path, reads and closes the response body, and returns
// the status code paired with the body bytes. Errors fail the test.
func (e *Env) Get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	resp, err := e.HTTP.Get(e.URL + path) //nolint:gosec,noctx // test helper against loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

// RequireOK GETs path, asserts the response is 200, and returns the body.
func (e *Env) RequireOK(t *testing.T, path string) []byte {
	t.Helper()
	status, body := e.Get(t, path)
	require.Equalf(t, http.StatusOK, status, "GET %s expected 200, got %d: %s", path, status, body)
	return body
}

// serveDaemon binds a loopback listener, runs the daemon against d in a
// background goroutine, and waits for /ping to return 200. Cleanup (server
// shutdown wait) is wired via t.Cleanup. Callers are responsible for closing d
// in a separately registered cleanup so LIFO ordering closes the DB after
// Serve returns.
func serveDaemon(t *testing.T, d *sqlitestore.Store, opts ...Option) (string, *http.Client, *daemon.EventBroadcaster) {
	t.Helper()
	// Bind the listener once and hand it directly to Server.Serve so no other
	// process can grab the port between bind and serve (the close-then-reopen
	// pattern has a TOCTOU race).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().(*net.TCPAddr).String() //nolint:forcetypeassert // net.Listen("tcp",...) always returns *net.TCPAddr

	bcast := daemon.NewEventBroadcaster()
	cfg := daemon.ServerConfig{
		DB:          d,
		StartedAt:   time.Now().UTC(),
		Broadcaster: bcast,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	applyClientAuthEnv(t, cfg)
	srv := daemon.NewServer(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, l)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Wait for /ping to answer with 200; if the daemon never becomes ready, or
	// if some other service won the port and answered with a non-200, fail
	// loudly here rather than letting the test report a confusing failure on
	// its first real request.
	url := "http://" + addr
	deadline := time.Now().Add(2 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(url + "/api/v1/ping") //nolint:noctx // polling loop; context would add noise without benefit
		if err == nil {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if status == http.StatusOK {
				ready = true
				break
			}
			lastErr = fmt.Errorf("unexpected /ping status %d", status)
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Truef(t, ready, "daemon did not become ready within 2s: %v", lastErr)
	return url, client, bcast
}

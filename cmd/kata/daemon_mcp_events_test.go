package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/mcpevents"
)

type failingKeyCheckStore struct{ db.Storage }

func (failingKeyCheckStore) PutMCPEventsKeyCheckIfAbsent(context.Context, string) (string, error) {
	return "", errors.New("store unavailable")
}

func (failingKeyCheckStore) MCPEventsKeyCheck(context.Context) (string, bool, error) {
	return "", false, errors.New("store unavailable")
}

func startMCPEventsForTest(t *testing.T, store db.Storage, enabled bool, keyPath string) (*mcpevents.Service, string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	workers := newDaemonWorkerGroup()
	cfg := config.DaemonConfig{MCPEvents: config.MCPEventsConfig{Enabled: enabled}}
	service, reason := startMCPEvents(ctx, workers, mcpEventsStartup{
		Config: cfg, KeyPath: keyPath, Store: store,
		Authority:   daemon.NewMCPEventsAuthority(daemon.ServerConfig{DB: store}),
		Broadcaster: daemon.NewMCPEventsSubscriber(daemon.NewEventBroadcaster()),
		Log:         log.New(io.Discard, "", 0),
	})
	stop := func() {
		cancel()
		joined, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer joinCancel()
		assert.True(t, workers.Wait(joined), "the Events worker stops with the daemon context")
	}
	t.Cleanup(stop)
	return service, reason, stop
}

// requireDaemonServes proves the rest of the daemon works with Events in the
// given state, and that the status route reports why Events is off.
func requireDaemonServes(t *testing.T, store db.Storage, service *mcpevents.Service, reason, wantReason string) {
	t.Helper()
	server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "daemon-token"}, MCPEvents: service, MCPEventsDisabledReason: reason})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	serve := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer daemon-token")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	assert.Equal(t, http.StatusOK, serve("/api/v1/ping").Code)
	assert.Equal(t, http.StatusOK, serve("/api/v1/projects").Code)
	status := serve("/api/v1/mcp-events/subscriptions")
	require.Equal(t, http.StatusOK, status.Code, status.Body.String())
	assert.Contains(t, status.Body.String(), `"enabled":false`)
	assert.Contains(t, status.Body.String(), `"reason":"`+wantReason+`"`)
}

func newMCPEventsStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestStartMCPEventsRunsWhenEnabledAndStaysOffOtherwise(t *testing.T) {
	store := newMCPEventsStore(t)

	service, reason, _ := startMCPEventsForTest(t, store, false, filepath.Join(t.TempDir(), "mcp-events.key"))
	assert.Nil(t, service, "disabled in config starts nothing")
	assert.Empty(t, reason)

	keyPath := filepath.Join(t.TempDir(), "runtime", "mcp-events.key")
	service, reason, stop := startMCPEventsForTest(t, store, true, keyPath)
	require.NotNil(t, service)
	assert.Empty(t, reason)
	assert.True(t, service.Availability().Enabled, service.Availability().Reason)
	assert.FileExists(t, keyPath, "the first start creates the key")
	stop()
}

func TestMCPEventsFailuresDisableOnlyEvents(t *testing.T) {
	t.Run("corrupt key", func(t *testing.T) {
		store := newMCPEventsStore(t)
		keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
		require.NoError(t, os.WriteFile(keyPath, []byte("short"), 0o600))
		service, reason, _ := startMCPEventsForTest(t, store, true, keyPath)
		require.NotNil(t, service)
		assert.Equal(t, mcpevents.ReasonKeyUnavailable, service.Availability().Reason)
		requireDaemonServes(t, store, service, reason, mcpevents.ReasonKeyUnavailable)
		content, err := os.ReadFile(keyPath) //nolint:gosec // Test-owned temporary key path.
		require.NoError(t, err)
		assert.Equal(t, "short", string(content), "a corrupt key is never replaced")
	})

	t.Run("missing key with an existing key check", func(t *testing.T) {
		store := newMCPEventsStore(t)
		// A first start under another key leaves the check behind, as a restored
		// backup does.
		first, _, stop := startMCPEventsForTest(t, store, true, filepath.Join(t.TempDir(), "other.key"))
		require.True(t, first.Availability().Enabled)
		stop()
		keyPath := filepath.Join(t.TempDir(), "mcp-events.key")
		service, reason, _ := startMCPEventsForTest(t, store, true, keyPath)
		require.NotNil(t, service)
		assert.Equal(t, mcpevents.ReasonKeyUnavailable, service.Availability().Reason)
		assert.NoFileExists(t, keyPath, "the key is never silently regenerated")
		requireDaemonServes(t, store, service, reason, mcpevents.ReasonKeyUnavailable)
	})

	t.Run("key mismatch", func(t *testing.T) {
		store := newMCPEventsStore(t)
		first, _, stop := startMCPEventsForTest(t, store, true, filepath.Join(t.TempDir(), "first.key"))
		require.True(t, first.Availability().Enabled)
		stop()
		other := filepath.Join(t.TempDir(), "second.key")
		_, err := mcpevents.NewFileKeyProvider(other).Create(t.Context())
		require.NoError(t, err)
		service, reason, _ := startMCPEventsForTest(t, store, true, other)
		require.NotNil(t, service)
		assert.Equal(t, mcpevents.ReasonKeyMismatch, service.Availability().Reason)
		requireDaemonServes(t, store, service, reason, mcpevents.ReasonKeyMismatch)
	})

	t.Run("service construction fails", func(t *testing.T) {
		store := newMCPEventsStore(t)
		failing := failingKeyCheckStore{Storage: store}
		service, reason, _ := startMCPEventsForTest(t, failing, true, filepath.Join(t.TempDir(), "mcp-events.key"))
		assert.Nil(t, service)
		assert.Equal(t, mcpevents.ReasonStartFailed, reason)
		requireDaemonServes(t, store, service, reason, mcpevents.ReasonStartFailed)
	})
}

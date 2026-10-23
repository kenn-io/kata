package mcpevents_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
)

func TestConcurrentActivationsUseEveryAvailableSlot(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		firstGate, secondGate := make(chan struct{}), make(chan struct{})
		entered := make(chan struct{}, 2)
		client, _ := mcpevents.FixtureWebhook(t, func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Challenge string `json:"challenge"`
			}
			_ = json.UnmarshalRead(req.Body, &body)
			entered <- struct{}{}
			if req.URL.Query().Get("route") == "first" {
				<-firstGate
			} else {
				<-secondGate
			}
			_ = json.MarshalWrite(w, map[string]string{"challenge": body.Challenge})
		})
		h := newHarnessWith(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.Limits.PrincipalActive = 2; cfg.Webhook = client })
		firstDone, secondDone := make(chan error, 1), make(chan error, 1)
		first := h.request("example-user/first")
		first.Delivery.URL = "https://example.com/events?route=first"
		go func() { _, err := h.subscribe(principalA, first); firstDone <- err }()
		receive(t, entered)
		second := h.request("example-user/second")
		second.Delivery.URL = "https://example.com/events?route=second"
		go func() { _, err := h.subscribe(principalA, second); secondDone <- err }()
		receive(t, entered)
		close(firstGate)
		firstErr := receive(t, firstDone)
		close(secondGate)
		require.NoError(t, firstErr)
		require.NoError(t, receive(t, secondDone))
		assert.Zero(t, h.svc.InflightCount())
	})
}

type orderedRefreshStore struct {
	db.Storage
	mu              sync.Mutex
	calls           int
	firstEntered    chan struct{}
	secondCommitted chan struct{}
}

func (s *orderedRefreshStore) RefreshMCPEventSubscription(ctx context.Context, in db.MCPEventRefresh) (db.MCPEventSubscription, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		close(s.firstEntered)
		select {
		case <-s.secondCommitted:
		case <-ctx.Done():
			return db.MCPEventSubscription{}, ctx.Err()
		}
	}
	row, err := s.Storage.RefreshMCPEventSubscription(ctx, in)
	if call == 2 {
		close(s.secondCommitted)
	}
	return row, err
}

func TestConcurrentSecretRotationsRetainImmediatelyPrecedingSecret(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		ordered := &orderedRefreshStore{Storage: store, firstEntered: make(chan struct{}), secondCommitted: make(chan struct{})}
		h := newHarness(t, ordered)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		req.Delivery.Secret = testSecret(2)
		done := make(chan error, 1)
		go func() { _, err := h.subscribe(principalA, req); done <- err }()
		receive(t, ordered.firstEntered)
		second := h.request("example-user")
		second.Delivery.Secret = testSecret(3)
		h.mustSubscribe(second)
		require.NoError(t, receive(t, done))
		row := h.row(first.ID)
		current, err := mcpevents.Open(h.key, row.ID, mcpevents.SecretRoleCurrent, row.SecretEnc)
		require.NoError(t, err)
		assert.Equal(t, testSecret(2), string(current))
		previous, err := mcpevents.Open(h.key, row.ID, mcpevents.SecretRolePrevious, row.PreviousSecretEnc)
		require.NoError(t, err)
		assert.Equal(t, testSecret(3), string(previous))
		require.NotNil(t, row.PreviousSecretUntil)
	})
}

package mcpevents_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"sync"
	"testing"
	"time"

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

type activationRefreshStore struct {
	db.Storage
	beforeActivation func()
	err              error
	after            db.MCPEventSubscription
}

func (s *activationRefreshStore) ActivateMCPEventSubscription(ctx context.Context, in db.MCPEventActivation) (db.MCPEventSubscription, error) {
	hook := s.beforeActivation
	if hook != nil {
		s.beforeActivation = nil
		hook()
	}
	row, err := s.Storage.ActivateMCPEventSubscription(ctx, in)
	if hook != nil {
		s.err = err
		s.after, _ = s.GetMCPEventSubscription(ctx, in.Subscription.ID)
	}
	return row, err
}

func TestActivationDoesNotOverwriteConcurrentRefresh(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		gated := &activationRefreshStore{Storage: store}
		h := newHarness(t, gated)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		before := seedDelivery(h, first.ID)
		// This recovery activation starts from a stale snapshot of a live row.
		req.Cursor = cursorString(h, before, before.Incarnation, before.ContinuityGeneration, 500)
		var refreshed db.MCPEventSubscription
		gated.beforeActivation = func() {
			h.clock.Advance(time.Second)
			refresh := h.request("example-user")
			refresh.Delivery.Secret = testSecret(9)
			refresh.TTLMS = mcpevents.TTLValue(time.Hour.Milliseconds())
			h.mustSubscribe(refresh)
			refreshed = h.row(first.ID)
		}
		h.mustSubscribe(req)
		assert.ErrorIs(t, gated.err, db.ErrMCPEventConflict, "the stale activation must retry")
		assert.Equal(t, refreshed, gated.after, "expiry, secret and pending delivery survive the stale write")
		assert.Equal(t, before.Generation, refreshed.Generation, "refresh preserves worker generation")
	})
}

package mcpevents_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
)

// A delivery retry must never extend the grant's lifetime or retain its quota.
func TestSweepExpiresGrantsBeforeTheirPendingRetry(t *testing.T) {
	for _, kind := range []string{"event", "gap"} {
		t.Run(kind, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				h, sk := newDelivery(t, store)
				sk.respond = always(http.StatusServiceUnavailable, "Retry-After", "3600")
				req := h.request("example-user")
				req.TTLMS = mcpevents.TTLValue((2 * time.Minute).Milliseconds())
				result := h.mustSubscribe(req)
				if kind == "event" {
					h.fake.addEvent(h.event(101))
				} else {
					h.fake.reset(150, 150)
				}
				h.drain()
				before := h.row(result.ID)
				require.Equal(t, db.MCPEventPendingKind(kind), before.PendingKind)
				require.NotNil(t, before.NextAttemptAt)
				require.True(t, before.NextAttemptAt.After(*before.ExpiresAt))

				h.clock.Advance(2 * time.Minute)
				require.NoError(t, h.svc.Sweep(t.Context()))
				after := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateEnded, after.State)
				assert.Equal(t, "expired", after.EndReason)
				assert.Equal(t, before.Generation+1, after.Generation)
				assert.Empty(t, after.PendingKind)
				assert.Empty(t, after.PendingMessageID)
				assert.Empty(t, after.PendingBody)
				assert.Nil(t, after.PendingNextCursor)
				assert.Nil(t, after.NextAttemptAt)
				assert.Empty(t, after.ClaimToken)
				assert.Nil(t, after.ClaimUntil)
				require.NotNil(t, after.EndedAt)
				assert.Equal(t, h.clock.Now(), *after.EndedAt)
				active, err := store.CountActiveMCPEventSubscriptions(t.Context())
				require.NoError(t, err)
				assert.Zero(t, active, "expired grants release quota before their retry is due")
				h.drainDue()
				assert.Equal(t, 1, sk.count(), "expiry emits no termination or retry callback")
			})
		})
	}
}

package mcpevents_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/mcpevents"
)

// fakeSubscriber stands in for the daemon broadcaster adapter.
type fakeSubscriber struct {
	mu         sync.Mutex
	channels   []chan struct{}
	subscribed chan int
	cancels    atomic.Int64
}

func newFakeSubscriber() *fakeSubscriber { return &fakeSubscriber{subscribed: make(chan int, 16)} }

func (f *fakeSubscriber) Subscribe() (<-chan struct{}, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{}, 1)
	f.channels = append(f.channels, ch)
	f.subscribed <- len(f.channels)
	return ch, func() { f.cancels.Add(1) }
}

// overflow closes the live channel, as the broadcaster does on overflow.
func (f *fakeSubscriber) overflow() {
	f.mu.Lock()
	defer f.mu.Unlock()
	close(f.channels[len(f.channels)-1])
}

func (f *fakeSubscriber) wake() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.channels[len(f.channels)-1] <- struct{}{}:
	default:
	}
}

func (f *fakeSubscriber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.channels)
}

// runService starts Run and returns a stop function that waits for it.
func runService(t *testing.T, svc *mcpevents.Service) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			assert.NoError(t, receive(t, done))
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestRunWakesOnTheBroadcasterAndResubscribesAfterOverflow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		subscriber := newFakeSubscriber()
		drained := make(chan struct{}, 32)
		h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) {
			cfg.Broadcaster = subscriber
			cfg.DrainHook = func() {
				select {
				case drained <- struct{}{}:
				default:
				}
			}
		})
		result := h.mustSubscribe(h.request("example-user"))
		stop := runService(t, h.svc)
		assert.Equal(t, 1, receive(t, subscriber.subscribed), "one service-level subscription")
		receive(t, drained)

		h.touch(result.ID)
		subscriber.wake()
		receive(t, sk.hitCh)

		// A change marked while the subscription is overflowing is picked up by
		// the drain that follows the resubscribe, not by the 5 s ticker (the
		// test ticker is an hour).
		for len(drained) > 0 {
			<-drained
		}
		h.touch(result.ID)
		subscriber.overflow()
		assert.Equal(t, 2, receive(t, subscriber.subscribed), "the closed subscription is re-established")
		receive(t, drained)
		got := receive(t, sk.hitCh)
		assert.NotEmpty(t, got.id)
		assert.Equal(t, 2, sk.count())
		assert.Equal(t, 2, subscriber.count(), "still exactly one live subscription")
		stop()
		assert.GreaterOrEqual(t, subscriber.cancels.Load(), int64(1))
	})
}

func TestRunDeliversOnTheFallbackTicker(t *testing.T) {
	for _, broadcaster := range []bool{false, true} {
		t.Run(fmt.Sprintf("broadcaster=%t", broadcaster), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				drained := make(chan struct{}, 1)
				h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) {
					cfg.PollInterval = 20 * time.Millisecond
					if broadcaster {
						cfg.Broadcaster = newFakeSubscriber()
					}
					cfg.DrainHook = func() {
						select {
						case drained <- struct{}{}:
						default:
						}
					}
				})
				runService(t, h.svc)
				receive(t, drained) // the initial cycle has no workers to kick another drain
				result := h.mustSubscribe(h.request("example-user"))
				h.touch(result.ID) // deliberately do not broadcast or Recheck
				receive(t, sk.hitCh)
			})
		})
	}
}

// A disabled service returns before any database work, so one backend is enough.
func TestRunReturnsPromptlyWhenEventsIsDisabled(t *testing.T) {
	subscriber := newFakeSubscriber()
	svc, err := mcpevents.NewService(t.Context(), mcpevents.ServiceConfig{Store: openSQLite(t), Broadcaster: subscriber})
	require.NoError(t, err)
	require.False(t, svc.Availability().Enabled)
	done := make(chan error, 1)
	go func() { done <- svc.Run(t.Context()) }()
	assert.NoError(t, receive(t, done), "a disabled service has nothing to run")
	assert.Zero(t, subscriber.count(), "and subscribes to nothing")
	assert.NoError(t, svc.Drain(t.Context()))
	assert.NoError(t, svc.Sweep(t.Context()))
}

func TestMismatchedKeyDisablesDeliveryAndClaimsNothing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)

		other, err := mcpevents.NewService(t.Context(), mcpevents.ServiceConfig{
			Store: store, Keys: newMemKeys(0x22), Authority: h.auth, Webhook: h.recv.client, Now: h.clock.Now,
		})
		require.NoError(t, err)
		require.False(t, other.Availability().Enabled)
		assert.Equal(t, mcpevents.ReasonKeyMismatch, other.Availability().Reason)
		done := make(chan error, 1)
		go func() { done <- other.Run(t.Context()) }()
		assert.NoError(t, receive(t, done))
		require.NoError(t, other.Drain(t.Context()))

		assert.Zero(t, sk.count())
		row := h.row(result.ID)
		assert.Empty(t, row.ClaimToken, "a mismatched service claims nothing")
		assert.Zero(t, row.AttemptCount)
	})
}

// A claimant that loses its claim cancels the request it has in flight.
func TestLostRenewalCancelsTheInFlightRequest(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.RenewInterval = 5 * time.Millisecond })
		entered, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
		sk.respond = nil
		h.recv.deliver = func(w http.ResponseWriter, r *http.Request, raw []byte) {
			sk.handle(w, r, raw)
			entered <- struct{}{}
			select {
			case <-r.Context().Done():
				cancelled <- struct{}{}
			case <-time.After(30 * time.Second):
			}
		}
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)

		drained := make(chan error, 1)
		go func() {
			h.svc.Recheck()
			drained <- h.svc.Drain(t.Context())
		}()
		receive(t, entered)
		claimed := h.row(result.ID)
		require.NotEmpty(t, claimed.ClaimToken)
		ok, err := h.store.ClaimMCPEventSubscription(t.Context(), db.MCPEventClaim{
			ID: result.ID, Incarnation: claimed.Incarnation, Generation: claimed.Generation, Token: "other-process",
			Now: h.clock.Now().Add(2 * time.Minute), Until: h.clock.Now().Add(3 * time.Minute),
		})
		require.NoError(t, err)
		require.True(t, ok)

		receive(t, cancelled)
		require.NoError(t, receive(t, drained))
		row := h.row(result.ID)
		assert.Equal(t, "other-process", row.ClaimToken, "the loser neither released nor settled")
		assert.Equal(t, db.MCPEventPendingEvent, row.PendingKind)
	})
}

// twoProcesses opens two stores on one PostgreSQL database, each with its own
// Service, key provider and receiver, as two daemons would.
type twoProcesses struct {
	a, b   *harness
	sinkA  *sink
	shared *fakeAuthorized
	clock  *testClock
}

func newTwoProcesses(t *testing.T, tune func(*mcpevents.ServiceConfig)) twoProcesses {
	t.Helper()
	if testing.Short() {
		t.Skip("requires postgres")
	}
	dsn, cleanup := pgstoretest.NewDatabase(t, t.Context())
	t.Cleanup(cleanup)
	open := func() db.Storage {
		store, err := pgstore.Open(t.Context(), dsn)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		return store
	}
	clock := newTestClock()
	fake := &fakeAuthorized{projects: []mcpevents.Project{{UID: testProjectA, ID: 1}}}
	build := func() (*harness, *sink) {
		h, sk := newDelivery(t, open(), func(cfg *mcpevents.ServiceConfig) {
			cfg.Now = clock.Now
			if tune != nil {
				tune(cfg)
			}
		})
		// Both processes share one database, one clock and one key.
		h.auth.authorized = fake
		h.clock = clock
		return h, sk
	}
	a, sinkA := build()
	b, _ := build()
	require.Equal(t, a.keys.key, b.keys.key)
	return twoProcesses{a: a, b: b, sinkA: sinkA, shared: fake, clock: clock}
}

// concurrencyReceiver counts overlapping requests per webhook-id.
type concurrencyReceiver struct {
	mu       sync.Mutex
	inflight map[string]int
	peak     int
	total    atomic.Int64
	entered  chan string
	gate     chan struct{}
}

func (c *concurrencyReceiver) serve(w http.ResponseWriter, r *http.Request, _ []byte) {
	id := r.Header.Get("Webhook-Id")
	c.mu.Lock()
	c.inflight[id]++
	c.peak = max(c.peak, c.inflight[id])
	c.mu.Unlock()
	c.total.Add(1)
	select {
	case c.entered <- id:
	default:
	}
	<-c.gate
	c.mu.Lock()
	c.inflight[id]--
	c.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func TestTwoProcessesNeverAttemptTheSamePendingItemConcurrently(t *testing.T) {
	p := newTwoProcesses(t, nil)
	recv := &concurrencyReceiver{inflight: map[string]int{}, entered: make(chan string, 8), gate: make(chan struct{})}
	p.a.recv.deliver = recv.serve
	p.b.recv.deliver = recv.serve
	result := p.a.mustSubscribe(p.a.request("example-user"))
	p.a.touch(result.ID)

	drainA := make(chan error, 1)
	go func() { drainA <- p.a.svc.Drain(t.Context()) }()
	receive(t, recv.entered)

	// The other process finds the row claimed and leaves it alone.
	require.NoError(t, p.b.svc.Drain(t.Context()))
	assert.Equal(t, int64(1), recv.total.Load(), "only the claimant sent")

	// A change during the attempt becomes the claimant's next ping.
	p.b.touch(result.ID)
	close(recv.gate)
	require.NoError(t, receive(t, drainA))
	assert.Equal(t, 1, recv.peak, "at most one concurrent attempt per pending item")
	assert.Equal(t, int64(2), recv.total.Load())
	row := p.a.row(result.ID)
	assert.False(t, row.Dirty)
	assert.Empty(t, row.PendingKind)
	assert.Empty(t, row.ClaimToken)
}

func TestTwoProcessesDiscardAStaleSettlement(t *testing.T) {
	p := newTwoProcesses(t, nil)
	releaseA := make(chan struct{})
	entered := make(chan struct{}, 1)
	var calls atomic.Int64
	p.a.recv.deliver = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		calls.Add(1)
		entered <- struct{}{}
		<-releaseA
		// The stale claimant's answer, if it were applied, would dead-letter
		// a ping that was delivered.
		w.WriteHeader(http.StatusGone)
	}
	p.b.recv.deliver = func(w http.ResponseWriter, _ *http.Request, raw []byte) {
		var body struct {
			Name string `json:"name"`
		}
		assert.NoError(t, json.Unmarshal(raw, &body))
		// Count only event attempts, so a control body cannot hide a
		// duplicate delivery.
		if body.Name == mcpevents.EventName {
			calls.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}
	result := p.a.mustSubscribe(p.a.request("example-user"))
	p.a.touch(result.ID)

	drainA := make(chan error, 1)
	go func() { drainA <- p.a.svc.Drain(t.Context()) }()
	receive(t, entered)

	// A stalls past its claim; B takes over, delivers and settles.
	p.clock.Advance(2 * time.Minute)
	require.NoError(t, p.b.svc.Drain(t.Context()))
	settled := p.a.row(result.ID)
	require.Empty(t, settled.PendingKind)
	require.Zero(t, settled.DeadLetterCount)

	close(releaseA)
	require.NoError(t, receive(t, drainA))
	row := p.a.row(result.ID)
	assert.Zero(t, row.DeadLetterCount, "the stale 410 settlement changed nothing")
	assert.Equal(t, db.MCPEventStateActive, row.State)
	assert.Empty(t, row.LastError)
	assert.Equal(t, settled.LastDeliveryAt, row.LastDeliveryAt)
	assert.Equal(t, int64(2), calls.Load())
}

func TestRenewIntervalStaysBelowTheClaimMargin(t *testing.T) {
	for name, tc := range map[string]struct {
		configured time.Duration
		want       func(ceiling time.Duration) time.Duration
	}{
		"unset uses the default":                      {0, func(time.Duration) time.Duration { return 20 * time.Second }},
		"a short interval stays":                      {time.Second, func(time.Duration) time.Duration { return time.Second }},
		"a long interval is clamped below the margin": {10 * time.Minute, nil},
		"the margin itself is clamped":                {45 * time.Second, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, ceiling := mcpevents.EffectiveRenewInterval(mcpevents.ServiceConfig{RenewInterval: tc.configured})
			if tc.want != nil {
				assert.Equal(t, tc.want(ceiling), got)
			}
			assert.Less(t, got, ceiling, "a renewal must land before the claim enters its margin")
			assert.Positive(t, got)
		})
	}
}

package agentsview

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
)

func TestAggregateCanonicalAndOverflow(t *testing.T) {
	zero, amount, max := int64(0), int64(42), int64(math.MaxInt64)
	cases := []struct {
		name                string
		sessions            []SessionUsage
		incomplete          bool
		state               string
		total               *int64
		estimated, overflow bool
	}{
		{name: "no sessions", state: "no_sessions"},
		{name: "unknown", sessions: []SessionUsage{{State: "not_found"}}, state: "unknown"},
		{name: "zero", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &zero}}, state: "complete", total: &zero},
		{name: "incomplete", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &amount, CostSource: "computed"}}, incomplete: true, state: "partial", total: &amount, estimated: true},
		{name: "deduplicated", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &amount}, {State: "priced", CanonicalSessionID: "a", CostMicrodollars: &amount}}, state: "complete", total: &amount},
		{name: "overflow", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &max}, {State: "priced", CanonicalSessionID: "b", CostMicrodollars: &amount}}, overflow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := aggregate(tc.sessions, tc.incomplete)
			if tc.overflow {
				require.ErrorIs(t, err, ErrTotalOverflow)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.state, got.State)
			require.Equal(t, tc.total, got.CostMicrodollars)
			require.Equal(t, tc.estimated, got.Estimated)
		})
	}
}

func TestCacheMissBackoffAndRefresh(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	client, err := NewClient(config.ResolvedAgentsView{URL: server.URL}, false)
	require.NoError(t, err)
	s := newGeneration(client)
	now := time.Now()
	s.now = func() time.Time { return now }
	lookup := func(refresh bool) {
		require.Equal(t, "not_found", s.lookup(t.Context(), make(chan struct{}, 4), "example", "id", refresh).State)
	}
	lookup(false)
	lookup(false)
	require.EqualValues(t, 1, requests.Load())
	now = now.Add(11 * time.Second)
	lookup(true)
	require.EqualValues(t, 2, requests.Load())
	now = now.Add(90 * time.Second)
	lookup(false)
	require.EqualValues(t, 2, requests.Load())
	now = now.Add(31 * time.Second)
	lookup(false)
	require.EqualValues(t, 3, requests.Load())
}

func TestNoSessionsDoesNotContactSource(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(server.Close)
	service, err := NewService(config.ResolvedAgentsView{URL: server.URL, BrowserURL: server.URL}, false)
	require.NoError(t, err)
	result, err := service.Usage(t.Context(), "run", cron.Summary{}, false)
	require.NoError(t, err)
	require.Equal(t, "no_sessions", result.State)
	require.Zero(t, requests.Load())
}

func TestReloadRetainsTargetAndClearsCacheForCredentialChange(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	cfg := config.ResolvedAgentsView{URL: server.URL, BrowserURL: server.URL, Token: "first"}
	service, err := NewService(cfg, false)
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "example", SessionID: "id"}}
	summary := cron.Summary{Sessions: &sessions}
	_, err = service.Usage(t.Context(), "run", summary, false)
	require.NoError(t, err)
	_, err = service.Usage(t.Context(), "run", summary, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, requests.Load())
	changed := cfg
	changed.URL = "https://other.example"
	swapped, err := service.Reload(changed, false)
	require.NoError(t, err)
	require.False(t, swapped)
	cfg.Token = "second"
	swapped, err = service.Reload(cfg, false)
	require.NoError(t, err)
	require.True(t, swapped)
	_, err = service.Usage(t.Context(), "run", summary, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())
}

func TestCanceledQueueWaitIsNotCached(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	client, err := NewClient(config.ResolvedAgentsView{URL: server.URL}, false)
	require.NoError(t, err)
	gen := newGeneration(client)
	slots := make(chan struct{}, 4)
	for range 4 {
		slots <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Equal(t, "unavailable", gen.lookup(ctx, slots, "example", "id", false).State)
	<-slots
	require.Equal(t, "not_found", gen.lookup(t.Context(), slots, "example", "id", false).State)
	require.EqualValues(t, 1, requests.Load())
}

func TestConcurrentIdenticalLookupsShareRequest(t *testing.T) {
	var requests atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(config.ResolvedAgentsView{URL: server.URL}, false)
	require.NoError(t, err)
	gen := newGeneration(client)
	slots := make(chan struct{}, 4)
	results := make(chan SessionUsage, 2)
	go func() { results <- gen.lookup(t.Context(), slots, "example", "id", false) }()
	<-entered
	go func() { results <- gen.lookup(t.Context(), slots, "example", "id", false) }()
	// Both callers can observe the same cached result even if scheduling makes
	// the second arrive after the first lookup has completed.
	close(release)
	require.Equal(t, "not_found", (<-results).State)
	require.Equal(t, "not_found", (<-results).State)
	require.EqualValues(t, 1, requests.Load())
}

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f usageRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCacheLifetimesAndBoundedEviction(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		ttl        time.Duration
	}{
		{"priced", `{"session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1},"cost_source":"reported"}`, 200, 5 * time.Minute},
		{"unpriced", `{"session_id":"id","has_cost":false,"has_token_data":true}`, 200, 5 * time.Minute},
		{"no_usage", `{"session_id":"id","has_cost":false,"has_token_data":false}`, 200, 5 * time.Minute},
		{"ambiguous", `{"error":{"code":"ambiguous_session_id","candidates":["archive~id"]}}`, 409, 5 * time.Minute},
		{"unauthorized", `{}`, 401, 15 * time.Second},
		{"unavailable", `{}`, 500, 15 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
			require.NoError(t, err)
			client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
			gen := newGeneration(client)
			now := time.Now()
			gen.now = func() time.Time { return now }
			slots := make(chan struct{}, 4)
			gen.lookup(t.Context(), slots, "example", "id", false)
			now = now.Add(tc.ttl - time.Nanosecond)
			gen.lookup(t.Context(), slots, "example", "id", false)
			require.EqualValues(t, 1, calls.Load())
			now = now.Add(time.Nanosecond)
			gen.lookup(t.Context(), slots, "example", "id", false)
			require.EqualValues(t, 2, calls.Load())
		})
	}
	var calls atomic.Int32
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	gen := newGeneration(client)
	slots := make(chan struct{}, 4)
	for i := 0; i < 2049; i++ {
		gen.lookup(t.Context(), slots, "example", fmt.Sprint(i), false)
	}
	require.Equal(t, 2048, gen.cache.Len())
	gen.lookup(t.Context(), slots, "example", "0", false)
	require.EqualValues(t, 2050, calls.Load())
}

func TestUsageLimitsConcurrentRequestsToFour(t *testing.T) {
	var active, maximum atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	gen := newGeneration(client)
	slots := make(chan struct{}, 4)
	done := make(chan SessionUsage, 8)
	for i := 0; i < 8; i++ {
		go func(i int) { done <- gen.lookup(t.Context(), slots, "example", fmt.Sprint(i), false) }(i)
	}
	for range 4 {
		<-entered
	}
	close(release)
	for range 8 {
		require.Equal(t, "not_found", (<-done).State)
	}
	require.EqualValues(t, 4, maximum.Load())
}

// Expiring the overall request budget is local to that request; it must not
// cache an upstream failure for the next reader.
func TestRequestBudgetExpiryDoesNotPoisonCache(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	gen := newGeneration(client)
	slots := make(chan struct{}, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan SessionUsage, 1)
	go func() { done <- gen.lookup(ctx, slots, "example", "id", false) }()
	<-entered
	require.Equal(t, "unavailable", (<-done).State)
	// Join any still-finishing flight before inspecting the next request.
	_, _, _ = gen.flights.Do("example\x00id", func() (any, error) { return nil, nil })
	require.Equal(t, "not_found", gen.lookup(t.Context(), slots, "example", "id", false).State)
	require.EqualValues(t, 2, calls.Load())
}

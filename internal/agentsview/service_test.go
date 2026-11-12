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
	"testing/synctest"
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
		{name: "unknown", sessions: []SessionUsage{{State: "not_found"}}, state: "unknown"},
		{name: "zero", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &zero}}, state: "complete", total: &zero},
		{name: "partial", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &amount, CostSource: "mixed"}, {State: "not_found"}}, state: "partial", total: &amount, estimated: true},
		{name: "duplicate maximum", sessions: []SessionUsage{{State: "priced", CanonicalSessionID: "a", CostMicrodollars: &max}, {State: "priced", CanonicalSessionID: "a", CostMicrodollars: &max}}, state: "complete", total: &max},
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
	// The oldest fact contributing to the response determines its freshness.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	service.current.now = func() time.Time { return now }
	oldest := now.Add(-2 * time.Minute)
	amount := int64(42)
	service.current.cache.Add("example\x00one", cacheEntry{fact: SessionUsage{State: "priced", CanonicalSessionID: "one", CostMicrodollars: &amount, CostSource: "reported"}, at: oldest, ttl: 5 * time.Minute})
	service.current.cache.Add("example\x00two", cacheEntry{fact: SessionUsage{State: "not_found"}, at: now.Add(-30 * time.Second), ttl: time.Minute})
	sessions := []cron.Session{{Agent: "example", SessionID: "one"}, {Agent: "example", SessionID: "two"}}
	result, err = service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
	require.NoError(t, err)
	require.Equal(t, oldest, result.FetchedAt)
	require.Zero(t, requests.Load())
}

func TestReloadRetainsTargetAndClearsCacheForCredentialChange(t *testing.T) {
	var requests atomic.Int32
	headers := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
	}))
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
	_, err = service.Usage(t.Context(), "run", summary, false)
	require.NoError(t, err)
	require.EqualValues(t, 1, requests.Load())
	cfg.Token = "second"
	swapped, err = service.Reload(cfg, false)
	require.NoError(t, err)
	require.True(t, swapped)
	_, err = service.Usage(t.Context(), "run", summary, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, requests.Load())
	require.Equal(t, "Bearer first", <-headers)
	require.Equal(t, "Bearer second", <-headers)
}

func TestConcurrentIdenticalLookupsShareRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		gen := testGeneration(t)
		gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			<-release
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		})
		slots := make(chan struct{}, 4)
		results := make(chan cacheEntry, 2)
		go func() { results <- gen.lookup(t.Context(), slots, "example", "id", false) }()
		synctest.Wait()
		go func() { results <- gen.lookup(t.Context(), slots, "example", "id", false) }()
		synctest.Wait()
		gotCalls := calls.Load()
		close(release)
		require.EqualValues(t, 1, gotCalls)
		require.Equal(t, "not_found", (<-results).fact.State)
		require.Equal(t, "not_found", (<-results).fact.State)
	})
}

func testGeneration(t *testing.T) *generation {
	t.Helper()
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	return newGeneration(client)
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
		{"not_found", `{}`, 404, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			gen := testGeneration(t)
			gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
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
			if tc.name == "not_found" {
				now = now.Add(10 * time.Second)
				gen.lookup(t.Context(), slots, "example", "id", true)
				require.EqualValues(t, 2, calls.Load())
				now = now.Add(time.Second)
				gen.lookup(t.Context(), slots, "example", "id", true)
				require.EqualValues(t, 3, calls.Load())
				now = now.Add(3*time.Minute + 59*time.Second)
				gen.lookup(t.Context(), slots, "example", "id", false)
				require.EqualValues(t, 3, calls.Load())
				now = now.Add(time.Second)
				gen.lookup(t.Context(), slots, "example", "id", false)
				require.EqualValues(t, 4, calls.Load())
			}
		})
	}
	var calls atomic.Int32
	gen := testGeneration(t)
	gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	slots := make(chan struct{}, 4)
	for i := 0; i < 2049; i++ {
		gen.lookup(t.Context(), slots, "example", fmt.Sprint(i), false)
	}
	require.Equal(t, 2048, gen.cache.Len())
	gen.lookup(t.Context(), slots, "example", "0", false)
	require.EqualValues(t, 2050, calls.Load())
}

func TestUsageLimitsConcurrentRequestsToFour(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var active, maximum atomic.Int32
		release := make(chan struct{})
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example", BrowserURL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		service.current.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if n <= old || maximum.CompareAndSwap(old, n) {
					break
				}
			}
			<-release
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		})
		sessions := make([]cron.Session, 8)
		for i := range sessions {
			sessions[i] = cron.Session{Agent: "example", SessionID: fmt.Sprint(i)}
		}
		done := make(chan error, 1)
		go func() {
			_, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
			done <- err
		}()
		synctest.Wait()
		gotMaximum := maximum.Load()
		close(release)
		require.NoError(t, <-done)
		require.EqualValues(t, 4, gotMaximum)
		require.EqualValues(t, 4, maximum.Load())
	})
}

// Expiring the overall request budget is local to that request; it must not
// cache an upstream failure for the next reader.
func TestRequestBudgetExpiryDoesNotPoisonCache(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	gen := testGeneration(t)
	gen.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})
	slots := make(chan struct{}, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan cacheEntry, 1)
	go func() { done <- gen.lookup(ctx, slots, "example", "id", false) }()
	<-entered
	require.Equal(t, "unavailable", (<-done).fact.State)
	// Join any still-finishing flight before inspecting the next request.
	_, _, _ = gen.flights.Do("example\x00id", func() (any, error) { return nil, nil })
	require.Equal(t, "not_found", gen.lookup(t.Context(), slots, "example", "id", false).fact.State)
	require.EqualValues(t, 2, calls.Load())
}

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
}

func TestUsageFetchedAtIsOldestFact(t *testing.T) {
	service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	service.current.now = func() time.Time { return now }
	oldest := now.Add(-2 * time.Minute)
	amount := int64(42)
	service.current.cache.Add("example\x00one", cacheEntry{fact: SessionUsage{State: "priced", CanonicalSessionID: "one", CostMicrodollars: &amount, CostSource: "reported"}, at: oldest, ttl: 5 * time.Minute})
	service.current.cache.Add("example\x00two", cacheEntry{fact: SessionUsage{State: "not_found"}, at: now.Add(-30 * time.Second), ttl: time.Minute})
	sessions := []cron.Session{{Agent: "example", SessionID: "two"}, {Agent: "example", SessionID: "one"}}
	result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
	require.NoError(t, err)
	require.Equal(t, oldest, result.FetchedAt)
}

func TestReloadRetainsTargetAndClearsCacheForCredentialChange(t *testing.T) {
	var requests atomic.Int32
	headers := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		headers <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, notFoundBody)
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

func testGeneration(t *testing.T) *generation {
	t.Helper()
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
	require.NoError(t, err)
	return newGeneration(client)
}

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f usageRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

const notFoundBody = `{"error":{"code":"session_not_found","message":"not found"}}`

func TestCacheLifetimes(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		ttl        time.Duration
	}{
		{"priced", `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1},"cost_source":"reported"}`, 200, 5 * time.Minute},
		{"unpriced", `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":true}`, 200, 5 * time.Minute},
		{"no_usage", `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":false}`, 200, 5 * time.Minute},
		{"ambiguous", `{"error":{"code":"ambiguous_session_id","candidates":["archive~id"]}}`, 409, 5 * time.Minute},
		{"unauthorized", `{}`, 401, 15 * time.Second},
		{"unavailable", `{}`, 500, 15 * time.Second},
		{"not_found", notFoundBody, 404, time.Minute},
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
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			slots := make(chan struct{}, 4)
			gen.lookup(ctx, slots, "claude", "id", false)
			now = now.Add(tc.ttl - time.Nanosecond)
			gen.lookup(ctx, slots, "claude", "id", false)
			require.EqualValues(t, 1, calls.Load())
			now = now.Add(time.Nanosecond)
			gen.lookup(ctx, slots, "claude", "id", false)
			require.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestRefreshAgeBoundary(t *testing.T) {
	gen := testGeneration(t)
	var calls atomic.Int32
	gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
	})
	now := time.Now()
	gen.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	slots := make(chan struct{}, 4)
	gen.lookup(ctx, slots, "example", "id", false)
	now = now.Add(10 * time.Second)
	gen.lookup(ctx, slots, "example", "id", true)
	require.EqualValues(t, 1, calls.Load())
	now = now.Add(time.Second)
	gen.lookup(ctx, slots, "example", "id", true)
	require.EqualValues(t, 2, calls.Load())
}

func TestCacheBoundedEviction(t *testing.T) {
	var calls atomic.Int32
	gen := testGeneration(t)
	gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
	})
	slots := make(chan struct{}, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for i := 0; i < 2049; i++ {
		gen.lookup(ctx, slots, "example", fmt.Sprint(i), false)
	}
	require.Equal(t, 2048, gen.cache.Len())
	gen.lookup(ctx, slots, "example", "0", false)
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
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
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
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		entered := make(chan struct{}, 1)
		gen := testGeneration(t)
		gen.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				entered <- struct{}{}
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
		})
		slots := make(chan struct{}, 4)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan cacheEntry, 1)
		go func() { done <- gen.lookup(ctx, slots, "example", "id", false) }()
		<-entered
		require.Equal(t, "unavailable", (<-done).fact.State)
		synctest.Wait()
		next, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		require.Equal(t, "not_found", gen.lookup(next, slots, "example", "id", false).fact.State)
		require.EqualValues(t, 2, calls.Load())
	})
}

func TestUsageAssemblesRecordedSession(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, returned, state, total string
	}{
		{"claude", "claude", "claude", "priced", "complete"},
		{"codex", "codex", "codex", "priced", "complete"},
		{"pi", "pi", "pi", "priced", "complete"},
		{"omp", "omp", "omp", "priced", "complete"},
		{"opencode", "opencode", "opencode", "priced", "complete"},
		{"overlapping raw ID", "opencode", "kilo", "unavailable", "unknown"},
		{"empty returned agent", "opencode", "", "unavailable", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, err := NewService(config.ResolvedAgentsView{URL: "https://api.example", BrowserURL: "https://browser.example/archive"}, false)
			require.NoError(t, err)
			id := "ses_example"
			if tc.recorded == "claude" || tc.recorded == "codex" {
				id = "00000000-0000-4000-8000-000000000001"
			}
			var path string
			service.current.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				path = r.URL.Path
				body := fmt.Sprintf(`{"agent":%q,"session_id":"archive~canonical","has_cost":true,"has_token_data":true,"cost":{"microdollars":1},"cost_source":"reported"}`, tc.returned)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})
			step := "inspect"
			sessions := []cron.Session{{Agent: tc.recorded, SessionID: id, Step: &step}}
			result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
			require.NoError(t, err)
			require.Equal(t, "run", result.RunUID)
			require.Len(t, result.Sessions, 1)
			session := result.Sessions[0]
			require.Equal(t, tc.recorded, session.Agent)
			require.Equal(t, id, session.SessionID)
			require.Equal(t, "/api/v1/sessions/"+id+"/usage", path)
			require.Equal(t, &step, session.Step)
			if tc.recorded == "opencode" {
				require.Equal(t, "https://browser.example/archive/sessions/opencode/ses_example", session.URL)
			}
			require.Equal(t, tc.state, session.State)
			require.Equal(t, tc.total, result.State)
			if tc.state == "priced" {
				require.Equal(t, "archive~canonical", session.CanonicalSessionID)
				require.NotNil(t, result.CostMicrodollars)
				require.EqualValues(t, 1, *result.CostMicrodollars)
			} else {
				require.Empty(t, session.CanonicalSessionID)
				require.Nil(t, session.CostMicrodollars)
				require.Nil(t, result.CostMicrodollars)
			}
		})
	}
}

func TestSharedLookupSurvivesFirstCallerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		var calls atomic.Int32
		release := make(chan struct{})
		service.current.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			<-release
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
		})
		sessions := []cron.Session{{Agent: "example", SessionID: "id"}}
		summary := cron.Summary{Sessions: &sessions}
		ctx, cancel := context.WithCancel(t.Context())
		first, second := make(chan RunUsage, 1), make(chan RunUsage, 1)
		go func() { result, _ := service.Usage(ctx, "run", summary, false); first <- result }()
		synctest.Wait()
		go func() { result, _ := service.Usage(t.Context(), "run", summary, false); second <- result }()
		synctest.Wait()
		cancel()
		require.Equal(t, "unavailable", (<-first).Sessions[0].State)
		close(release)
		require.Equal(t, "not_found", (<-second).Sessions[0].State)
		require.EqualValues(t, 1, calls.Load())
	})
}

func TestUsageQueueGetsFullLookupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		service.current.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			select {
			case <-time.After(1500 * time.Millisecond):
				return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})
		sessions := make([]cron.Session, 8)
		for i := range sessions {
			sessions[i] = cron.Session{Agent: "example", SessionID: fmt.Sprint(i)}
		}
		result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
		require.NoError(t, err)
		require.Len(t, result.Sessions, 8)
		for _, session := range result.Sessions {
			require.Equal(t, "not_found", session.State)
		}
	})
}

func TestLookupTimeoutCachedForFifteenSeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gen := testGeneration(t)
		var calls atomic.Int32
		gen.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		slots := make(chan struct{}, 4)
		lookup := func() cacheEntry {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			return gen.lookup(ctx, slots, "example", "id", false)
		}
		start := time.Now()
		require.Equal(t, "unavailable", lookup().fact.State)
		require.Equal(t, 2*time.Second, time.Since(start))
		time.Sleep(15*time.Second - time.Nanosecond)
		lookup()
		require.EqualValues(t, 1, calls.Load())
		time.Sleep(time.Nanosecond)
		lookup()
		require.EqualValues(t, 2, calls.Load())
	})
}

func TestUsageRequestBudgetExpiresWhileQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		var calls atomic.Int32
		service.current.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
		})
		for i := 0; i < 4; i++ {
			service.slots <- struct{}{}
		}
		sessions := []cron.Session{{Agent: "example", SessionID: "id"}}
		start := time.Now()
		result, err := service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
		require.NoError(t, err)
		require.Equal(t, 5*time.Second, time.Since(start))
		require.Equal(t, "unavailable", result.Sessions[0].State)
		require.Zero(t, calls.Load())
		// Finish the expired flight while the queue is still saturated.
		synctest.Wait()
		for i := 0; i < 4; i++ {
			<-service.slots
		}
		result, err = service.Usage(t.Context(), "run", cron.Summary{Sessions: &sessions}, false)
		require.NoError(t, err)
		require.Equal(t, "not_found", result.Sessions[0].State)
		require.EqualValues(t, 1, calls.Load())
	})
}

func TestNotFoundBackoffCapsAtFifteenMinutes(t *testing.T) {
	gen := testGeneration(t)
	var calls atomic.Int32
	gen.client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(notFoundBody)), Header: make(http.Header)}, nil
	})
	now := time.Now()
	gen.now = func() time.Time { return now }
	slots := make(chan struct{}, 4)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, ttl := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		gen.lookup(ctx, slots, "example", "id", false)
		expected := calls.Load()
		now = now.Add(ttl - time.Nanosecond)
		gen.lookup(ctx, slots, "example", "id", false)
		require.Equal(t, expected, calls.Load())
		now = now.Add(time.Nanosecond)
	}
	gen.lookup(ctx, slots, "example", "id", false)
	require.EqualValues(t, 7, calls.Load())
}

func TestProbeDeadlines(t *testing.T) {
	t.Run("lookup expiry", testProbeUsesSlotsAndLookupDeadline)
	t.Run("queue expiry", testProbeDeadlineIncludesSlotQueue)
}

func testProbeUsesSlotsAndLookupDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		var calls atomic.Int32
		service.current.client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		for i := 0; i < 4; i++ {
			service.slots <- struct{}{}
		}
		done := make(chan string, 1)
		start := time.Now()
		go func() { done <- service.Probe(t.Context()) }()
		synctest.Wait()
		require.Zero(t, calls.Load())
		<-service.slots
		require.Equal(t, "unreachable", <-done)
		require.Equal(t, 2*time.Second, time.Since(start))
		require.EqualValues(t, 1, calls.Load())
		for i := 0; i < 3; i++ {
			<-service.slots
		}
	})
}

func testProbeDeadlineIncludesSlotQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := NewService(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
		require.NoError(t, err)
		for i := 0; i < 4; i++ {
			service.slots <- struct{}{}
		}
		start := time.Now()
		require.Equal(t, "unreachable", service.Probe(t.Context()))
		require.Equal(t, 2*time.Second, time.Since(start))
		for i := 0; i < 4; i++ {
			<-service.slots
		}
	})
}

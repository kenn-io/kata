package agentsview

import (
	"math"
	"net/http"
	"net/http/httptest"
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

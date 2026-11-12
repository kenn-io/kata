package agentsview

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestCronUsageUnauthorizedStatusIgnoresBodyFailure(t *testing.T) {
	// The usage status contract maps both 401 and 403 to unauthorized,
	// regardless of whether their response bodies can be read.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, tc := range []struct {
			name string
			body func() io.Reader
		}{
			{"oversized", func() io.Reader { return strings.NewReader(strings.Repeat("x", (1<<20)+1)) }},
			{"unreadable", func() io.Reader { return iotest.ErrReader(errors.New("fixture read failure")) }},
		} {
			t.Run(http.StatusText(status)+"/"+tc.name, func(t *testing.T) {
				client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example"}, false)
				require.NoError(t, err)
				client.http.Transport = usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Body: io.NopCloser(tc.body()), Header: make(http.Header)}, nil
				})
				require.Equal(t, "unauthorized", client.Lookup(t.Context(), "fixture").State)
			})
		}
	}
}

func TestLookupSessionStates(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body, state string
	}{
		{"priced zero", 200, `{"agent":"claude","session_id":"archive~id","has_cost":true,"has_token_data":true,"cost":{"microdollars":0},"cost_source":"reported","project":"private"}`, "priced"},
		{"unpriced", 200, `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":true,"unpriced_models":["model"]}`, "unpriced"},
		{"no usage", 200, `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":false}`, "no_usage"},
		{"not found", 404, `{"error":{"code":"session_not_found","message":"not found"}}`, "not_found"},
		{"html 404", 404, `<html>not found</html>`, "unavailable"},
		{"wrong endpoint 404", 404, `{}`, "unavailable"},
		{"malformed envelope 404", 404, `{"error":{"code":"session_not_found"}}`, "unavailable"},
		{"ambiguous", 409, `{"error":{"code":"ambiguous_session_id","candidates":["archive~id"]}}`, "ambiguous"},
		{"negative cost", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":-1},"cost_source":"reported"}`, "unavailable"},
		{"negative tokens", 200, `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":true,"total_output_tokens":-1}`, "unavailable"},
		{"negative subagents", 200, `{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":true,"subagent_count":-1}`, "unavailable"},
		{"missing cost", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost_source":"reported"}`, "unavailable"},
		{"invalid source", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1},"cost_source":"other"}`, "unavailable"},
		{"fractional", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1.5},"cost_source":"reported"}`, "unavailable"},
		{"overflow", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":9223372036854775808},"cost_source":"reported"}`, "unavailable"},
		{"server error", 500, `private upstream detail`, "unavailable"},
		{"oversized", 200, `{"agent":"claude","session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1},"cost_source":"reported","padding":"` + strings.Repeat("x", 1<<20) + `"}`, "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(config.ResolvedAgentsView{URL: server.URL + "/prefix", Token: "fixture-token"}, false)
			require.NoError(t, err)
			got := client.Lookup(t.Context(), "raw")
			require.Equal(t, tc.state, got.State)
			if tc.name == "priced zero" {
				require.NotNil(t, got.CostMicrodollars)
				require.Zero(t, *got.CostMicrodollars)
			}
		})
	}
}

func TestLookupDoesNotFollowRedirect(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			requests.Add(1)
			return
		}
		http.Redirect(w, r, "/redirected", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	client, err := NewClient(config.ResolvedAgentsView{URL: origin.URL, Token: "fixture-token"}, false)
	require.NoError(t, err)
	require.Equal(t, "unavailable", client.Lookup(t.Context(), "raw").State)
	require.Zero(t, requests.Load())
}

func TestDoctorProbeValidatesNotFoundEnvelope(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
		want   string
	}{
		{`{"error":{"code":"session_not_found","message":"not found"}}`, 404, "ok"},
		{`{"error":{"code":"ambiguous_session_id","candidates":["archive~id"]}}`, 409, "ok"},
		{`{"error":{"code":"foreign_conflict","message":"conflict"}}`, 409, "not_agentsview"},
		{`{"agent":"claude","session_id":"id","has_cost":false,"has_token_data":false}`, 200, "ok"},
		{`not found`, 404, "not_agentsview"},
		{`{}`, 401, "unauthorized"},
		{`{}`, 500, "unreachable"},
		{`{}`, http.StatusTooManyRequests, "unreachable"},
	} {
		t.Run(tc.want+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client, err := NewClient(config.ResolvedAgentsView{URL: server.URL}, false)
			require.NoError(t, err)
			require.Equal(t, tc.want, client.Probe(t.Context()))
		})
	}
}

func TestLookupEscapesRawSessionID(t *testing.T) {
	client, err := NewClient(config.ResolvedAgentsView{URL: "https://agentsview.example/prefix", Token: "fixture-token"}, false)
	require.NoError(t, err)
	var path string
	client.http.Transport = usageRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		path = r.URL.EscapedPath()
		require.Equal(t, "subagents=true", r.URL.RawQuery)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"session_not_found","message":"not found"}}`)), Header: make(http.Header)}, nil
	})
	require.Equal(t, "not_found", client.Lookup(t.Context(), "raw/id?part").State)
	require.Equal(t, "/prefix/api/v1/sessions/raw%2Fid%3Fpart/usage", path)
}

package agentsview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestLookupSessionStates(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body, state string
	}{
		{"priced zero", 200, `{"session_id":"archive~id","has_cost":true,"has_token_data":true,"cost":{"microdollars":0},"cost_source":"reported","project":"private"}`, "priced"},
		{"unpriced", 200, `{"session_id":"id","has_cost":false,"has_token_data":true,"unpriced_models":["model"]}`, "unpriced"},
		{"no usage", 200, `{"session_id":"id","has_cost":false,"has_token_data":false}`, "no_usage"},
		{"not found", 404, `{}`, "not_found"},
		{"ambiguous", 409, `{"error":{"code":"ambiguous_session_id","candidates":["archive~id"]}}`, "ambiguous"},
		{"unauthorized", 403, `private upstream detail`, "unauthorized"},
		{"fractional", 200, `{"session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":1.5}}`, "unavailable"},
		{"overflow", 200, `{"session_id":"id","has_cost":true,"has_token_data":true,"cost":{"microdollars":9223372036854775808}}`, "unavailable"},
		{"server error", 500, `private upstream detail`, "unavailable"},
		{"oversized", 200, strings.Repeat("x", (1<<20)+1), "unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/prefix/api/v1/sessions/raw/usage", r.URL.Path)
				require.Equal(t, "subagents=true", r.URL.RawQuery)
				require.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
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

func TestLookupRejectsRedirectWithoutSendingToken(t *testing.T) {
	var requests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
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
		{`not found`, 404, "not_agentsview"},
		{`{}`, 401, "unauthorized"},
		{`{}`, 500, "unreachable"},
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

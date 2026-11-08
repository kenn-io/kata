package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Contract: selected daemon is the only CLI network destination. Redirects
// and receiver-like responses cannot become proof of root ingest acceptance.
func TestAgentsViewHTTPRefusesRedirectCompressionAndOversize(t *testing.T) {
	for _, state := range []string{"redirect", "compression", "oversize", "error-content"} {
		t.Run(state, func(t *testing.T) {
			var escaped int
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped++; w.WriteHeader(200) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch state {
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
				case "compression":
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = io.WriteString(w, "private body")
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat("x", int(agentsViewRecordLimit)+1))
				case "error-content":
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"message":"private transcript bytes"}`)
				}
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			port := &agentsViewHTTPPort{api: daemonAPI{client: client, baseURL: server.URL}}
			_, err := port.request(t.Context(), http.MethodPost, "/synthetic", bytes.NewReader([]byte("selected")), nil, 10*time.Second)
			require.Error(t, err)
			require.Zero(t, escaped)
			require.NotContains(t, err.Error(), "private")
		})
	}
}
func TestAgentsViewDefaultAdapterCannotPretendSuccess(t *testing.T) {
	workflow, err := newLiveAgentsViewWorkflow(t.Context())
	require.ErrorContains(t, err, "integration is unavailable")
	require.Nil(t, workflow)
}
func TestAgentsViewHistoryUsesExactEffectiveCloseAndAssociation(t *testing.T) {
	_, _, consent, _ := agentsViewWorkflowFixture(t)
	history := struct {
		Events      []map[string]any `json:"events"`
		NextAfterID int64            `json:"next_after_id"`
		Effective   string           `json:"effective_close_event_uid"`
	}{[]map[string]any{{"event_uid": "close-a", "event_id": 1, "type": "issue.closed", "issue_uid": "issue-a", "project_uid": "project-a", "payload": map[string]any{"agentsview_association": consent.Association}}}, 1, "close-a"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/projects/2/events", r.URL.Path)
		require.Equal(t, "issue-a", r.URL.Query().Get("issue"))
		_ = json.MarshalWrite(w, history)
	}))
	defer server.Close()
	port := &agentsViewHTTPPort{api: daemonAPI{client: server.Client(), baseURL: server.URL}, projectID: 2, issueUID: "issue-a"}
	event, err := port.Original(t.Context(), "example#abcd", "close-a")
	require.NoError(t, err)
	require.True(t, consent.matchesEvent(event, "close-a"))
	history.Effective = "later-close"
	_, err = port.Original(t.Context(), "example#abcd", "close-a")
	require.Error(t, err)
}

// Keep compile-time interface coverage independent from the runtime integration.
var _ agentsViewWorkflowPort = (*agentsViewHTTPPort)(nil)

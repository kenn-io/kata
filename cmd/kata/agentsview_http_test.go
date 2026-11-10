package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
)

// Contract: selected daemon is the only CLI network destination. Redirects
// and receiver-like responses cannot become proof of root ingest acceptance.
func TestAgentsViewHTTPRefusesCompressionAndOversize(t *testing.T) {
	for _, state := range []string{"compression", "oversize", "error-content"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch state {
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
			port := &agentsViewHTTPPort{api: daemonAPI{client: client, baseURL: server.URL}}
			_, err := port.request(t.Context(), http.MethodPost, "/synthetic", bytes.NewReader([]byte("selected")), nil, 10*time.Second)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private")
		})
	}
}

func TestAgentsViewDefaultAdapterCannotPretendSuccess(t *testing.T) {
	workflow, err := newLiveAgentsViewWorkflow(t.Context())
	require.ErrorContains(t, err, "sharing is unavailable")
	require.Nil(t, workflow)
}

func TestAgentsViewHistoryUsesExactEffectiveCloseAndAssociation(t *testing.T) {
	_, _, consent, _ := agentsViewWorkflowFixture(t)
	history := struct {
		Events      []map[string]any `json:"events"`
		NextAfterID int64            `json:"next_after_id"`
		Effective   string           `json:"effective_close_event_uid"`
	}{[]map[string]any{{"event_uid": "close-a", "event_id": 1, "type": "issue.closed", "issue_uid": "issue-a", "project_uid": "project-a", "payload": map[string]any{"agentsview_association": consent.Association, "reason": strings.Repeat("x", 32<<10)}}}, 1, "close-a"}
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

func TestAgentsViewHTTPMissingGeneratedAssociationCannotClose(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(500) }))
	defer server.Close()
	port := &agentsViewHTTPPort{api: daemonAPI{client: server.Client(), baseURL: server.URL}, projectID: 2, issueUID: "issue-a"}
	_, err := port.Close(t.Context(), "example#abcd", map[string]any{"agentsview_protocol": "relay-v1"}, nil)
	require.ErrorContains(t, err, "sharing is unavailable")
	require.Zero(t, calls)
}

func TestAgentsViewOriginalAssociationRejectsAddedFields(t *testing.T) {
	_, _, consent, _ := agentsViewWorkflowFixture(t)
	raw, err := json.Marshal(consent.Association)
	require.NoError(t, err)
	raw = append(raw[:len(raw)-1], []byte(`,"unknown_authority":"other"}`)...)
	payload := append([]byte(`{"agentsview_association":`), raw...)
	payload = append(payload, '}')
	_, err = decodeAgentsViewEventAssociation(payload)
	require.Error(t, err)
}

type agentsViewRejectProof struct{}

func (agentsViewRejectProof) Discovery(context.Context, []byte, string, string) (agentsViewSelection, error) {
	return agentsViewSelection{}, errors.New("test has no pinned discovery")
}

func (agentsViewRejectProof) Receipt(context.Context, []byte, agentsViewConsent, string) error {
	return errors.New("test has no signed receipt")
}

func TestAgentsViewConstructedAdapterNeverFollowsRedirect(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	ref := createIssue(t, env, pid, "selected issue")
	flags.Workspace = dir
	escaped := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { escaped++; w.WriteHeader(500) }))
	defer target.Close()
	upstream, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(upstream); r.Out.Host = upstream.Host }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript-service") {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(307)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	ctx := contextWithBaseURL(t.Context(), server.URL)
	port, err := newAgentsViewHTTPPort(ctx, ref, agentsViewRejectProof{})
	require.NoError(t, err)
	_, err = port.Select(ctx, ref)
	require.ErrorContains(t, err, "HTTP 307")
	require.Zero(t, escaped)
}

func TestAgentsViewAttachmentMetadataHasControlDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		deadline, ok := r.Context().Deadline()
		require.True(t, ok)
		require.True(t, deadline.Before(time.Now().Add(11*time.Second)))
		return nil, context.Canceled
	})}
	port := &agentsViewHTTPPort{api: daemonAPI{client: client, baseURL: "http://127.0.0.1:12345"}, projectID: 2, issueUID: "issue-a"}
	_, err := port.ResolveEvidence(ctx, []api.Evidence{{Type: api.EvidenceAttachment, AttachmentID: "suffix"}})
	require.Error(t, err)
	require.Equal(t, 1, requests)
}

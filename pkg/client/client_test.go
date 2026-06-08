package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/pkg/client/generated"
)

func TestNewWithHTTPClientNormalizesBaseURL(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL+"/", server.Client())
	require.NoError(t, err)

	resp, err := api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "/api/v1/ping", gotPath)
}

func TestNewWithBearerAddsAuthorization(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	defer server.Close()

	api, err := NewWithBearer(t.Context(), server.URL, "secret-token")
	require.NoError(t, err)

	_, err = api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "Bearer secret-token", gotAuth)
}

func TestNewWithGlobalAuthAddsEnvAuthorization(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "env-token")

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	defer server.Close()

	api, err := NewWithGlobalAuth(t.Context(), server.URL)
	require.NoError(t, err)

	_, err = api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "Bearer env-token", gotAuth)
}

func TestNewForTargetAllowsPlaintextPrivateNetworkOptOut(t *testing.T) {
	api, err := NewForTarget(t.Context(), "http://daemon.example:7777", TargetAuth{
		Token:         "secret-token",
		AllowInsecure: true,
	})
	require.NoError(t, err)
	require.NotNil(t, api)
}

func TestNewWithTrustedActorHeader(t *testing.T) {
	var gotActor string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = r.Header.Get("X-Kata-Actor")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	defer server.Close()

	api, err := New(server.URL, WithTrustedActor("X-Kata-Actor", "alice"))
	require.NoError(t, err)

	_, err = api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "alice", gotActor)
}

func TestNewWithTrustedActorTrimsOnce(t *testing.T) {
	var gotActor string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor = r.Header.Get("X-Kata-Actor")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"service":"kata","version":"test"}`))
	}))
	defer server.Close()

	api, err := New(server.URL, WithTrustedActor(" X-Kata-Actor ", " alice "))
	require.NoError(t, err)

	_, err = api.PingWithResponse(t.Context())
	require.NoError(t, err)

	assert.Equal(t, "alice", gotActor)
}

func TestGeneratedArbitraryJSONMapsRoundTrip(t *testing.T) {
	var envelope generated.ErrorEnvelope
	err := json.Unmarshal([]byte(`{
		"status": 400,
		"error": {
			"code": "validation",
			"message": "bad metadata",
			"data": {
				"area": "work",
				"remove": null,
				"nested": {"ok": true},
				"items": [1, "two"]
			}
		}
	}`), &envelope)
	require.NoError(t, err)

	assert.Equal(t, "work", envelope.ErrorData.Data["area"])
	assert.Nil(t, envelope.ErrorData.Data["remove"])
	assert.Equal(t, map[string]any{"ok": true}, envelope.ErrorData.Data["nested"])
	assert.Equal(t, []any{float64(1), "two"}, envelope.ErrorData.Data["items"])

	patch := generated.PatchIssueMetadataRequestBody{
		Actor: "tester",
		Patch: map[string]any{
			"area":   "work",
			"remove": nil,
		},
	}
	raw, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.JSONEq(t, `{"actor":"tester","patch":{"area":"work","remove":null}}`, string(raw))
}

func TestGeneratedClientEscapesPathParams(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":true,"events":[]}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	_, err = api.DeleteIssue(t.Context(), &generated.DeleteIssueRequestOptions{
		PathParams: &generated.DeleteIssuePath{
			ProjectID: 7,
			Ref:       "victim/actions/purge?x=#fragment",
		},
		Body: &generated.DeleteIssueBody{
			Actor: "tester",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/projects/7/issues/victim%2Factions%2Fpurge%3Fx=%23fragment/actions/delete", gotPath)
}

func TestRawGeneratedClientEscapesPathParams(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deleted":true,"events":[]}`))
	}))
	defer server.Close()

	api, err := generated.NewDefaultClient(server.URL, runtime.WithHTTPClient(contextDoer{client: server.Client()}))
	require.NoError(t, err)

	_, err = api.DeleteIssue(t.Context(), &generated.DeleteIssueRequestOptions{
		PathParams: &generated.DeleteIssuePath{
			ProjectID: 7,
			Ref:       "victim/actions/purge?x=#fragment",
		},
		Body: &generated.DeleteIssueBody{
			Actor: "tester",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/projects/7/issues/victim%2Factions%2Fpurge%3Fx=%23fragment/actions/delete", gotPath)
}

func TestGeneratedStreamEventsUsesInt64Cursors(t *testing.T) {
	afterID := int64(1 << 40)
	projectID := int64(1 << 41)
	lastEventID := int64(1 << 42)

	query := generated.StreamEventsQuery{
		AfterID:   &afterID,
		ProjectID: &projectID,
	}
	headers := generated.StreamEventsHeaders{
		LastEventID: &lastEventID,
	}

	assert.Equal(t, afterID, *query.AfterID)
	assert.Equal(t, projectID, *query.ProjectID)
	assert.Equal(t, lastEventID, *headers.LastEventID)
}

func TestGeneratedClientUsesRepeatedQueryKeysForArrayParams(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"actors":[],"events":[],"range":{"since":"2026-01-01T00:00:00Z","until":"2026-01-02T00:00:00Z"}}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	_, err = api.DigestGlobal(t.Context(), &generated.DigestGlobalRequestOptions{
		Query: &generated.DigestGlobalQuery{
			Since: "2026-01-01T00:00:00Z",
			Actor: []string{"alice", "bob"},
		},
	})
	require.NoError(t, err)

	values := mustParseQuery(t, gotQuery)
	assert.Equal(t, []string{"alice", "bob"}, values["actor"])
	assert.NotContains(t, gotQuery, "alice%2Cbob")
}

func TestStreamEventsRawSetsAcceptAndDoesNotBuffer(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	var gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), testStreamTimeout)
	defer cancel()
	resp, err := api.StreamEventsRaw(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", gotAccept)
	_, err = io.ReadAll(io.LimitReader(resp.Body, 0))
	require.NoError(t, err)
}

func TestStreamEventsGeneratedMethodsSetAccept(t *testing.T) {
	var gotAccept []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = append(gotAccept, r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	_, err = api.StreamEvents(t.Context(), nil)
	require.NoError(t, err)
	_, err = api.StreamEventsWithResponse(t.Context(), nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"text/event-stream", "text/event-stream"}, gotAccept)
}

func TestStreamEventsRawClosesBodyOnError(t *testing.T) {
	body := &closeTrackingBody{}
	api, err := NewWithHTTPClient("http://daemon.example", &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			assert.Equal(t, "text/event-stream", req.Header.Get("Accept"))
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     make(http.Header),
				Body:       body,
				Request:    req,
			}, nil
		}),
	})
	require.NoError(t, err)

	resp, err := api.StreamEventsRaw(t.Context(), nil)

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.True(t, body.closed)
}

func TestGeneratedErrorEnvelopeIncludesDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":400,"error":{"code":"validation","message":"bad input"}}`))
	}))
	defer server.Close()

	api, err := NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)

	_, err = api.Ping(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "validation")
	assert.Contains(t, err.Error(), "bad input")
	assert.NotContains(t, err.Error(), "unmapped client error")
}

func TestGeneratedRecurrenceTemplateUpdateMarshalsEmptyPatchValues(t *testing.T) {
	labels := []string{}
	metadata := map[string]any{}
	body := generated.PatchRecurrenceRequestBody{
		Actor: "tester",
		Template: &generated.RecurrenceTemplateUpdateInput{
			Labels:   &labels,
			Metadata: &metadata,
		},
	}

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	assert.JSONEq(t, `{"actor":"tester","template":{"labels":[],"metadata":{}}}`, string(raw))
}

func TestGeneratedPollEventsPreservesPayload(t *testing.T) {
	raw := []byte(`{
		"events": [{
			"actor": "tester",
			"content_hash": "hash",
			"created_at": "2026-01-01T00:00:00Z",
			"event_id": 7,
			"event_uid": "evt_01",
			"origin_instance_uid": "origin_01",
			"payload": {"labels":["bug"],"nested":{"ok":true}},
			"project_id": 1,
			"project_name": "spoke-project",
			"project_uid": "proj_01",
			"type": "issue.updated"
		}],
		"next_after_id": 7,
		"reset_required": false
	}`)
	var body generated.PollEventsBody
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Len(t, body.Events, 1)

	assert.JSONEq(t, `{"labels":["bug"],"nested":{"ok":true}}`, string(body.Events[0].Payload))
}

func TestGeneratedFederationIngestEventPreservesPayload(t *testing.T) {
	payload := json.RawMessage(`{"items":[{"id":1}],"source":"hub-project"}`)
	event := generated.FederationIngestEventEnvelope{
		Actor:             "tester",
		ContentHash:       "hash",
		CreatedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EventID:           7,
		EventUID:          "evt_01",
		HlcPhysicalMs:     1,
		OriginInstanceUID: "origin_01",
		Payload:           payload,
		ProjectName:       "spoke-project",
		ProjectUID:        "proj_01",
		Type:              "issue.updated",
	}

	raw, err := json.Marshal(event)
	require.NoError(t, err)
	var encoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &encoded))
	assert.JSONEq(t, string(payload), string(encoded["payload"]))

	var decoded generated.FederationIngestEventEnvelope
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.JSONEq(t, string(payload), string(decoded.Payload))
}

func mustParseQuery(t *testing.T, raw string) map[string][]string {
	t.Helper()
	values, err := url.ParseQuery(raw)
	require.NoError(t, err)
	return values
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type closeTrackingBody struct {
	closed bool
}

func (b *closeTrackingBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

const testStreamTimeout = 2 * time.Second

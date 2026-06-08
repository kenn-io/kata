package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

const testStreamTimeout = 2 * time.Second

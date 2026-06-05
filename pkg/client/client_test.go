package client

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

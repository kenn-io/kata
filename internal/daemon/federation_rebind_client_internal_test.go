package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestFederationRebindPreflightUsesEnrollmentTokenAndPreservesPath(t *testing.T) {
	const (
		enrollmentToken = "enrollment-secret"
		adminToken      = "catalog-admin-secret"
		globalToken     = "spoke-daemon-secret"
	)
	t.Setenv("KATA_AUTH_TOKEN", globalToken)
	var requestPath, authorization string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.EscapedPath()
		authorization = r.Header.Get("Authorization")
		assert.NotContains(t, authorization, adminToken)
		assert.NotContains(t, authorization, globalToken)
		_, _ = w.Write([]byte(`{"project_id":41,"project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX"}`))
	}))
	t.Cleanup(server.Close)

	originalFactory := newFederationRebindHTTPClient
	newFederationRebindHTTPClient = func(
		_ context.Context, _ string, token string,
	) (*http.Client, error) {
		assert.Equal(t, enrollmentToken, token)
		assert.NotEqual(t, adminToken, token)
		assert.NotEqual(t, globalToken, token)
		return federationRebindTestBearerClient(t, server.Client(), server.URL, token), nil
	}
	t.Cleanup(func() { newFederationRebindHTTPClient = originalFactory })

	metadata, err := fetchFederationRebindMetadata(
		context.Background(), server.URL+"/reverse-proxy", enrollmentToken, 41,
	)

	require.NoError(t, err)
	assert.Equal(t, "/reverse-proxy/api/v1/projects/41/federation/metadata", requestPath)
	assert.Equal(t, "Bearer "+enrollmentToken, authorization)
	assert.Equal(t, int64(41), metadata.ProjectID)
}

func TestFederationRebindPreflightDoesNotCleanConfiguredPathPrefix(t *testing.T) {
	var requestURI string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		_, _ = w.Write([]byte(`{"project_id":41,"project_uid":"01HZNQ7VFPK1XGD8R5MABCD4EX"}`))
	}))
	t.Cleanup(server.Close)

	originalFactory := newFederationRebindHTTPClient
	newFederationRebindHTTPClient = func(
		_ context.Context, _ string, token string,
	) (*http.Client, error) {
		return federationRebindTestBearerClient(t, server.Client(), server.URL, token), nil
	}
	t.Cleanup(func() { newFederationRebindHTTPClient = originalFactory })

	_, err := fetchFederationRebindMetadata(
		context.Background(), server.URL+"/proxy//segment/../mount", "enrollment-secret", 41,
	)

	require.NoError(t, err)
	assert.Equal(
		t,
		"/proxy//segment/../mount/api/v1/projects/41/federation/metadata",
		requestURI,
	)
}

func TestFederationRebindPreflightPinsRedirectOrigin(t *testing.T) {
	reachedTarget := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reachedTarget = true
	}))
	t.Cleanup(target.Close)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	originalFactory := newFederationRebindHTTPClient
	newFederationRebindHTTPClient = func(
		context.Context, string, string,
	) (*http.Client, error) {
		return federationRebindTestBearerClient(t, source.Client(), source.URL, "enrollment-secret"), nil
	}
	t.Cleanup(func() { newFederationRebindHTTPClient = originalFactory })

	_, err := fetchFederationRebindMetadata(
		context.Background(), source.URL, "enrollment-secret", 41,
	)

	require.Error(t, err)
	assert.False(t, reachedTarget)
}

func TestFederationRebindPreflightBoundsRemoteErrors(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("remote-secret-body", 1024)))
	}))
	t.Cleanup(server.Close)

	originalFactory := newFederationRebindHTTPClient
	newFederationRebindHTTPClient = func(
		context.Context, string, string,
	) (*http.Client, error) {
		return federationRebindTestBearerClient(t, server.Client(), server.URL, "enrollment-secret"), nil
	}
	t.Cleanup(func() { newFederationRebindHTTPClient = originalFactory })

	_, err := fetchFederationRebindMetadata(
		context.Background(), server.URL, "enrollment-secret", 41,
	)

	require.ErrorIs(t, err, ErrFederationReplicaHubUnavailable)
	assert.NotContains(t, err.Error(), "remote-secret-body")
	assert.Less(t, len(err.Error()), 256)
}

func TestFederationRebindPreflightClassifiesEnrollmentRejection(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	originalFactory := newFederationRebindHTTPClient
	newFederationRebindHTTPClient = func(
		context.Context, string, string,
	) (*http.Client, error) {
		return federationRebindTestBearerClient(t, server.Client(), server.URL, "enrollment-secret"), nil
	}
	t.Cleanup(func() { newFederationRebindHTTPClient = originalFactory })

	_, err := fetchFederationRebindMetadata(
		context.Background(), server.URL, "enrollment-secret", 41,
	)

	require.ErrorIs(t, err, ErrFederationReplicaCredentialConflict)
	assert.False(t, errors.Is(err, ErrFederationReplicaHubUnavailable))
}

func federationRebindTestBearerClient(
	t *testing.T, httpClient *http.Client, baseURL, token string,
) *http.Client {
	t.Helper()
	origin, err := config.CanonicalHTTPOrigin(baseURL)
	require.NoError(t, err)
	httpClient.Transport = config.BearerTransport(httpClient.Transport, token, origin)
	return httpClient
}

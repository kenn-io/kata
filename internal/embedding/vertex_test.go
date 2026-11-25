package embedding

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kit/embedconfig"
)

func routeGoogleToken(t *testing.T, endpoint string) {
	t.Helper()
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "oauth2.googleapis.com" {
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
			r.Host = target.Host
		}
		return previous.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previous })
}

func TestVertexEmbeddingCredentials(t *testing.T) {
	for _, mode := range []string{"token", "vertex", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			var tokenRequests, vertexRequests int
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tokenRequests++
				require.NoError(t, r.ParseForm())
				refresh := r.Form.Get("refresh_token")
				w.Header().Set("Content-Type", "application/json")
				if mode == "token" && refresh == "bad" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"example-private-detail"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": refresh, "token_type": "Bearer", "expires_in": 3600})
			}))
			t.Cleanup(token.Close)
			routeGoogleToken(t, token.URL)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				vertexRequests++
				require.Equal(t, "/publishers/google/models/gemini-embedding-001:predict", r.URL.Path)
				if r.Header.Get("Authorization") == "Bearer bad" {
					status := http.StatusUnauthorized
					if mode == "forbidden" {
						status = http.StatusForbidden
					}
					w.WriteHeader(status)
					return
				}
				require.Equal(t, "Bearer good", r.Header.Get("Authorization"))
				_, _ = w.Write([]byte(`{"predictions":[{"embeddings":{"values":[3,4]}}]}`))
			}))
			t.Cleanup(endpoint.Close)
			path := filepath.Join(t.TempDir(), "credentials.json")
			write := func(refresh string) {
				body := fmt.Sprintf(`{"type":"authorized_user","client_id":"example","client_secret":"example","refresh_token":%q}`, refresh)
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			}
			write("bad")
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
			cfg := config.EmbeddingsConfig{API: embedconfig.APIVertex, BaseURL: endpoint.URL + "/publishers/google/models", Model: "gemini-embedding-001", Dims: 2, BatchSize: 9}
			client, err := New(ClientConfig(cfg))
			require.NoError(t, err)
			oldRevision := client.credentialRevision
			_, err = client.Embed(t.Context(), []string{"document"})
			var credentialErr *CredentialError
			require.ErrorAs(t, err, &credentialErr)
			if mode == "forbidden" {
				require.Contains(t, credentialErr.Reason, "permissions and model access")
			}
			require.NotContains(t, err.Error(), "example-private-detail")
			require.NotContains(t, client.CredentialHealth().LastError, "example-private-detail")
			require.Equal(t, "rejected", client.CredentialHealth().Credential)
			client.credentialSucceeded(oldRevision)
			require.Equal(t, "rejected", client.CredentialHealth().Credential)
			write("good")
			_, err = client.Embed(t.Context(), []string{"document"})
			require.NoError(t, err)
			require.Equal(t, "ok", client.CredentialHealth().Credential)
			require.Empty(t, client.CredentialHealth().LastError)
			require.Equal(t, 2, tokenRequests)
			require.GreaterOrEqual(t, vertexRequests, 1)
		})
	}
}

func TestVertexEmbeddingMissingCredentials(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/missing/example-credentials.json")
	client, err := New(Config{API: embedconfig.APIVertex, BaseURL: "https://example.com/publishers/google/models", Model: "gemini-embedding-001"})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), []string{"document"})
	var credentialErr *CredentialError
	require.ErrorAs(t, err, &credentialErr)
	require.Equal(t, "missing", client.CredentialHealth().Credential)
}

func TestVertexEmbeddingTemporaryTokenFailure(t *testing.T) {
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	t.Cleanup(token.Close)
	routeGoogleToken(t, token.URL)
	path := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"type":"authorized_user","client_id":"example","client_secret":"example","refresh_token":"example"}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	client, err := New(Config{API: embedconfig.APIVertex, BaseURL: "https://example.com/publishers/google/models", Model: "gemini-embedding-001"})
	require.NoError(t, err)
	_, err = client.Embed(t.Context(), []string{"document"})
	require.Error(t, err)
	var credentialErr *CredentialError
	require.NotErrorAs(t, err, &credentialErr)
	require.Equal(t, "ok", client.CredentialHealth().Credential)
	require.NotNil(t, client.google)
}

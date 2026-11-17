package githubsync

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

// appTestTransport fakes GitHub's token endpoint and API. It counts mints,
// records API tokens, rejects token requests not signed by key when set, and
// answers 401 to the rejected token.
type appTestTransport struct {
	mu       sync.Mutex
	mints    int
	tokens   []string
	expiry   time.Duration
	key      *rsa.PublicKey
	rejected string
}

func (rt *appTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	auth := req.Header.Get("Authorization")
	status, body := http.StatusNoContent, ""
	switch {
	case req.URL.Path != "/app/installations/456/access_tokens":
		rt.tokens = append(rt.tokens, auth)
		if auth == rt.rejected {
			status = http.StatusUnauthorized
		}
	case rt.key != nil && !appTestSignedBy(rt.key, strings.TrimPrefix(auth, "Bearer ")):
		status = http.StatusUnauthorized
	default:
		rt.mints++
		status = http.StatusCreated
		body = fmt.Sprintf(`{"token":"token-%d","expires_at":%q}`, rt.mints, time.Now().Add(rt.expiry).UTC().Format(time.RFC3339))
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func appTestSignedBy(key *rsa.PublicKey, jwt string) bool {
	parts := strings.Split(jwt, ".")
	signature, err := base64.RawURLEncoding.DecodeString(parts[len(parts)-1])
	digest := sha256.Sum256([]byte(strings.Join(parts[:len(parts)-1], ".")))
	return err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil
}

func writeAppTestKey(t *testing.T, path string) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0600))
	return key
}

func appTestResolver(t *testing.T) (*CredentialResolver, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	writeAppTestKey(t, path)
	return NewCredentialResolver(config.GitHubSyncConfig{Apps: []config.GitHubAppConfig{{Owner: "example-owner", AppID: 123, InstallationID: 456, PrivateKeyPath: path}}}, nil), path
}

// appTestSession opens one session of the binding, as a refresh does, and
// makes one API request.
func appTestSession(ctx context.Context, resolver *CredentialResolver, base http.RoundTripper) (int, error) {
	transport, err := resolver.Transport(ctx, Binding{Host: "github.com", Owner: "example-owner", Repo: "first"}, base)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/example-owner/first/issues", nil)
	if err != nil {
		return 0, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	return resp.StatusCode, resp.Body.Close()
}

func TestCredentialResolverAppReusesTokenAcrossSessions(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &appTestTransport{expiry: time.Hour}
	for range 2 {
		_, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, base.mints)
	assert.Equal(t, []string{"token token-1", "token token-1"}, base.tokens)
}

func TestCredentialResolverAppConcurrentSessionsMintOnce(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &appTestTransport{expiry: time.Hour}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := appTestSession(t.Context(), resolver, base)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, base.mints)
}

func TestCredentialResolverAppRemintsNearExpiry(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &appTestTransport{expiry: 30 * time.Second}
	for range 2 {
		_, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"token token-1", "token token-2"}, base.tokens)
}

func TestCredentialResolverAppEvictsRejectedToken(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &appTestTransport{expiry: time.Hour, rejected: "token token-1"}
	for _, want := range []int{http.StatusUnauthorized, http.StatusNoContent} {
		status, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
		assert.Equal(t, want, status)
	}
	assert.Equal(t, 2, base.mints)
}

func TestCredentialResolverAppReadsRotatedKeyAfterFailedMint(t *testing.T) {
	resolver, path := appTestResolver(t)
	rotated := filepath.Join(t.TempDir(), "rotated.pem")
	base := &appTestTransport{expiry: time.Hour, key: &writeAppTestKey(t, rotated).PublicKey}
	_, err := appTestSession(t.Context(), resolver, base)
	require.Error(t, err)
	require.NoError(t, os.Rename(rotated, path))
	_, err = appTestSession(t.Context(), resolver, base)
	require.NoError(t, err)
	assert.Equal(t, 1, base.mints)
}

func TestCredentialResolverPrefersAppOverEnvAndGH(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte("gh-token\n")},
		},
	}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
		Apps: []config.GitHubAppConfig{
			{
				Host:           "GitHub.Example",
				Owner:          "Example-Owner",
				AppID:          123,
				InstallationID: 456,
				PrivateKeyPath: "/secure/example-app.pem",
			},
		},
	}, runner)

	kind, err := resolver.ResolveKind(t.Context(), Binding{
		Host:  "github.example",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.Equal(t, CredentialKind("app"), kind)
	assert.Empty(t, runner.calls)
}

func TestCredentialResolverDoesNotUseAppForDifferentOwner(t *testing.T) {
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
		Apps: []config.GitHubAppConfig{
			{
				Owner:          "example-owner",
				AppID:          123,
				InstallationID: 456,
				PrivateKeyPath: "/secure/example-app.pem",
			},
		},
	}, runner)

	kind, err := resolver.ResolveKind(t.Context(), Binding{
		Host:  "github.com",
		Owner: "other-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.Equal(t, CredentialKind("env"), kind)
	assert.Empty(t, runner.calls)
}

func TestCredentialResolverUsesConfiguredEnvToken(t *testing.T) {
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{}
	next := &recordingRoundTripper{}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
	}, runner)
	resolver.baseTransport = next

	rt, kind, err := resolver.transport(t.Context(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, CredentialKind("env"), kind)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/example-owner/example-repo/issues", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, next.requests, 1)
	assert.Equal(t, "Bearer env-token", next.requests[0].Header.Get("Authorization"))
	assert.Empty(t, req.Header.Get("Authorization"), "transport must not mutate the caller's request")
	assert.Empty(t, runner.calls)
}

func TestCredentialResolverDoesNotUseDefaultEnvTokenForEnterpriseHost(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte("enterprise-gh-token\n")},
		},
	}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
	}, runner)

	kind, err := resolver.ResolveKind(t.Context(), Binding{
		Host:  "github.example",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.Equal(t, CredentialKind("gh"), kind)
	require.Len(t, runner.calls, 1)
	assert.Equal(t, []string{"auth", "token", "--hostname", "github.example"}, runner.calls[0].args)
}

func TestCredentialResolverUsesEnvTokenForConfiguredEnterpriseTokenHost(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{ //nolint:gosec // TokenHost is a hostname selector, not credential material.
		TokenEnv:  tokenEnv,
		TokenHost: " GitHub.Example ",
	}, runner)

	kind, err := resolver.ResolveKind(t.Context(), Binding{
		Host:  "github.example",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	assert.Equal(t, CredentialKind("env"), kind)
	assert.Empty(t, runner.calls)
}

func TestCredentialResolverTransportWrapsSuppliedBaseTransport(t *testing.T) {
	tokenEnv := exampleGitHubTokenEnv()
	t.Setenv(tokenEnv, "env-token")
	runner := &fakeCommandRunner{}
	fallback := &recordingRoundTripper{}
	supplied := &recordingRoundTripper{}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: tokenEnv,
	}, runner)
	resolver.baseTransport = fallback

	rt, err := resolver.Transport(t.Context(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, supplied)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.github.com/repos/example-owner/example-repo/issues", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Len(t, supplied.requests, 1)
	assert.Equal(t, "Bearer env-token", supplied.requests[0].Header.Get("Authorization"))
	assert.Empty(t, fallback.requests)
	assert.Empty(t, runner.calls)
}

func TestCredentialResolverCallsGHAuthTokenForEachResolve(t *testing.T) {
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{stdout: []byte("first-gh-token\n")},
			{stdout: []byte("second-gh-token\n")},
		},
	}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{}, runner)
	binding := Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}

	first, err := resolver.ResolveKind(t.Context(), binding)
	require.NoError(t, err)
	second, err := resolver.ResolveKind(t.Context(), binding)
	require.NoError(t, err)

	assert.Equal(t, CredentialKind("gh"), first)
	assert.Equal(t, CredentialKind("gh"), second)
	require.Len(t, runner.calls, 2)
	assert.Equal(t, []string{"auth", "token", "--hostname", "github.com"}, runner.calls[0].args)
	assert.Equal(t, []string{"auth", "token", "--hostname", "github.com"}, runner.calls[1].args)
}

func TestCredentialResolverNoCredentialsErrorNamesRemedies(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	runner := &fakeCommandRunner{
		responses: []fakeCommandResponse{
			{err: assert.AnError},
		},
	}
	resolver := NewCredentialResolver(config.GitHubSyncConfig{
		TokenEnv: exampleGitHubTokenEnv(),
	}, runner)

	_, err := resolver.ResolveKind(t.Context(), Binding{
		Host:  "github.example",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.Error(t, err)

	assert.Contains(t, err.Error(), "[[github_sync.app]]")
	assert.Contains(t, err.Error(), exampleGitHubTokenEnv())
	assert.Contains(t, err.Error(), "`gh auth login --hostname github.example`")
}

func TestRedactTokenMaterialRedactsGitHubTokens(t *testing.T) {
	token := "g" + "hp_" + "exampleSecretMaterial"

	message := redactTokenMaterial("request failed with token " + token)

	assert.Contains(t, message, "[redacted]")
	assert.NotContains(t, message, token)
}

func TestRedactTokenMaterialRedactsPrivateKeys(t *testing.T) {
	privateKey := "-----BEGIN RSA " + "PRIVATE" + " KEY-----\nexample-secret-key-material\n-----END RSA " + "PRIVATE" + " KEY-----"

	message := redactTokenMaterial("failed with key:\n" + privateKey)

	assert.Contains(t, message, "[redacted]")
	assert.NotContains(t, message, "BEGIN RSA PRIVATE KEY")
	assert.NotContains(t, message, "example-secret-key-material")
	assert.NotContains(t, message, "END RSA PRIVATE KEY")
}

func TestOriginGuardAllowsExpectedGitHubAPITargets(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	tests := []struct {
		name    string
		binding Binding
		url     string
		body    string
	}{
		{
			name:    "github rest",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/repos/example-owner/example-repo/issues",
		},
		{
			name:    "github graphql",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/graphql",
			body:    parentGraphQLAuthBody(t, "example-owner", "example-repo"),
		},
		{
			name:    "enterprise rest",
			binding: Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://github.example/api/v3/repos/example-owner/example-repo/issues",
		},
		{
			name:    "enterprise graphql",
			binding: Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://github.example/api/graphql",
			body:    parentGraphQLAuthBody(t, "example-owner", "example-repo"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &recordingRoundTripper{}
			guard := newOriginGuardTransport(tt.binding, next)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tt.url, strings.NewReader(tt.body))
			require.NoError(t, err)

			resp, err := guard.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			assert.Len(t, next.requests, 1)
		})
	}
}

func TestOriginGuardBlocksWrongTargetBeforeNextTransport(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	tests := []struct {
		name    string
		binding Binding
		url     string
		body    string
	}{
		{
			name:    "github credential to enterprise host",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://github.example/api/v3/repos/example-owner/example-repo/issues",
		},
		{
			name:    "enterprise credential to github host",
			binding: Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/graphql",
		},
		{
			name:    "wrong owner",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/repos/other-owner/example-repo/issues",
		},
		{
			name:    "wrong repo",
			binding: Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://github.example/api/v3/repos/example-owner/other-repo/issues",
		},
		{
			name:    "plaintext",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "http://api.github.com/repos/example-owner/example-repo/issues",
		},
		{
			name:    "dot segment",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/repos/example-owner/example-repo/%2e%2e/other-repo/issues",
		},
		{
			name:    "unscoped github graphql",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/graphql",
			body:    `{"query":"query { repository(owner: \"other-owner\", name: \"example-repo\") { issues(first: 1) { nodes { number } } } }"}`,
		},
		{
			name:    "wrong graphql variables",
			binding: Binding{Host: "github.example", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://github.example/api/graphql",
			body:    parentGraphQLAuthBody(t, "other-owner", "example-repo"),
		},
		{
			name:    "extra graphql root field",
			binding: Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"},
			url:     "https://api.github.com/graphql",
			body:    `{"query":"query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { issues(first: 1) { nodes { number } } } node(id: \"N_other\") { id } }","variables":{"owner":"example-owner","repo":"example-repo"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &recordingRoundTripper{}
			guard := newOriginGuardTransport(tt.binding, next)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tt.url, strings.NewReader(tt.body))
			require.NoError(t, err)

			resp, err := guard.RoundTrip(req)

			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Contains(t, err.Error(), "GitHub credential egress blocked")
			assert.Empty(t, next.requests)
		})
	}
}

func allowGitHubEnterpriseHost(t *testing.T, host string) {
	t.Helper()
	t.Setenv("KATA_GITHUB_SYNC_ALLOWED_HOSTS", host)
}

func parentGraphQLAuthBody(t testing.TB, owner, repo string) string {
	t.Helper()
	body, err := json.Marshal(parentGraphQLRequest{
		Query: parentGraphQLQuery,
		Variables: map[string]any{
			"owner": owner,
			"repo":  repo,
			"after": nil,
		},
	})
	require.NoError(t, err)
	return string(body)
}

func exampleGitHubTokenEnv() string {
	return "EXAMPLE_" + "GITHUB_TOKEN"
}

type fakeCommandRunner struct {
	calls     []fakeCommandCall
	responses []fakeCommandResponse
}

func (r *fakeCommandRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	r.calls = append(r.calls, fakeCommandCall{name: name, args: append([]string(nil), args...)})
	if len(r.responses) == 0 {
		return nil, nil, assert.AnError
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.stdout, response.stderr, response.err
}

type fakeCommandCall struct {
	name string
	args []string
}

type fakeCommandResponse struct {
	stdout []byte
	stderr []byte
	err    error
}

type recordingRoundTripper struct {
	requests []*http.Request
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requests = append(rt.requests, req)
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestGraphQLIncrementalParentQueryGuardAcceptsOnlyGeneratedShape(t *testing.T) {
	query := `query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { i0: issue(number: 3) { number fullDatabaseId parent { number fullDatabaseId } } } }`
	assert.True(t, graphQLQueryMatchesParentQuery(query))
	statusQuery := `query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) { i0: issue(number: 3) { fullDatabaseId number state stateReason closedAt updatedAt repository { databaseId } } } }`
	assert.True(t, graphQLQueryMatchesParentQuery(statusQuery))
	assert.False(t, graphQLQueryMatchesParentQuery(strings.Replace(statusQuery, "stateReason", "title stateReason", 1)))
	for _, bad := range []string{
		strings.Replace(query, "number: 3", "number: -3", 1),
		strings.Replace(query, "i0:", "i1:", 1),
		strings.Replace(query, "fullDatabaseId parent", "title fullDatabaseId parent", 1),
		strings.Replace(query, "query(", "mutation(", 1),
		strings.Replace(query, "name: $repo", "name: \"foreign-repo\"", 1),
	} {
		assert.False(t, graphQLQueryMatchesParentQuery(bad))
	}
}

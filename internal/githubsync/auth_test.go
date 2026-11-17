package githubsync

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

// appTestTransport fakes GitHub's token endpoint and API. It counts mints,
// keeps each mint's JWT and each API request's Authorization header, answers
// 401 to the rejected token, and fails API requests with apiErr when set.
type appTestTransport struct {
	expiry   time.Duration
	rejected string
	apiErr   error
	jwts     []string
	tokens   []string
}

func (rt *appTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	auth := req.Header.Get("Authorization")
	status, body := http.StatusNoContent, ""
	if req.URL.Path == "/app/installations/456/access_tokens" {
		rt.jwts = append(rt.jwts, strings.TrimPrefix(auth, "Bearer "))
		status = http.StatusCreated
		body = fmt.Sprintf(`{"token":"token-%d","expires_at":%q}`, len(rt.jwts), time.Now().Add(rt.expiry).UTC().Format(time.RFC3339))
	} else {
		rt.tokens = append(rt.tokens, auth)
		if rt.apiErr != nil {
			return nil, rt.apiErr
		}
		if auth == rt.rejected {
			status = http.StatusUnauthorized
		}
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// appTestFuncTransport is a RoundTripper whose dynamic type cannot be a map key.
type appTestFuncTransport func(*http.Request) (*http.Response, error)

func (f appTestFuncTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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
// makes one REST read.
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
	resolver, path := appTestResolver(t)
	base := &appTestTransport{expiry: time.Hour}
	for i := range 3 {
		_, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
		if i == 0 {
			require.NoError(t, os.Remove(path))
		}
	}
	assert.Len(t, base.jwts, 1)
	assert.Equal(t, []string{"token token-1", "token token-1", "token token-1"}, base.tokens)
}

func TestCredentialResolverAppMintsAgainAfterRejectedToken(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &appTestTransport{expiry: time.Hour, rejected: "token token-1"}
	for _, want := range []int{http.StatusUnauthorized, http.StatusNoContent} {
		status, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
		assert.Equal(t, want, status)
	}
	assert.Equal(t, []string{"token token-1", "token token-2"}, base.tokens)

	resolver, _ = appTestResolver(t)
	base = &appTestTransport{expiry: time.Hour, apiErr: errors.New("connection reset")}
	for range 2 {
		_, err := appTestSession(t.Context(), resolver, base)
		require.ErrorIs(t, err, base.apiErr)
	}
	assert.Len(t, base.jwts, 1)
}

func TestCredentialResolverAppReadsKeyFileOnlyToMint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolver, path := appTestResolver(t)
		base := &appTestTransport{expiry: time.Hour}
		_, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)

		require.NoError(t, os.Remove(path))
		time.Sleep(time.Hour)
		_, err = appTestSession(t.Context(), resolver, base)
		require.ErrorContains(t, err, "read GitHub App private key")

		rotated := writeAppTestKey(t, path)
		_, err = appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
		require.Len(t, base.jwts, 2)
		_, err = jwt.Parse(base.jwts[1], func(*jwt.Token) (any, error) { return &rotated.PublicKey, nil })
		require.NoError(t, err, "the new mint is signed with the replaced key")
	})
}

func TestCredentialResolverAppRejectsBadKeyOnSetup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
		want    string
	}{
		{"missing", func(t *testing.T, path string) { require.NoError(t, os.Remove(path)) }, "read GitHub App private key"},
		{"invalid", func(t *testing.T, path string) { require.NoError(t, os.WriteFile(path, []byte("invalid"), 0600)) }, "parse GitHub App private key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver, path := appTestResolver(t)
			tc.prepare(t, path)
			_, err := resolver.Transport(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "first"}, &appTestTransport{expiry: time.Hour})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

type stalledAppTransport struct {
	started chan struct{}
	release chan struct{}
}

func (rt *stalledAppTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	default:
	}
	close(rt.started)
	<-rt.release
	return nil, errors.New("mint stopped")
}

func TestCredentialResolverAppBindingsDoNotBlockEachOther(t *testing.T) {
	resolver, _ := appTestResolver(t)
	base := &stalledAppTransport{started: make(chan struct{}), release: make(chan struct{})}
	first, err := resolver.Transport(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "first"}, base)
	require.NoError(t, err)
	second, err := resolver.Transport(t.Context(), Binding{Host: "github.com", Owner: "example-owner", Repo: "second"}, base)
	require.NoError(t, err)
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	request := func(ctx context.Context, tr http.RoundTripper, repo string, done chan<- error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/example-owner/"+repo+"/issues", nil)
		_, err := tr.RoundTrip(req)
		done <- err
	}
	go request(t.Context(), first, "first", firstDone)
	<-base.started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	go request(ctx, second, "second", secondDone)
	completed := false
	select {
	case err := <-secondDone:
		completed = true
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Error("canceled request blocked behind another binding's mint")
	}
	close(base.release)
	<-firstDone
	if !completed {
		<-secondDone
	}
}

func TestCredentialResolverAppUsesEachSuppliedBase(t *testing.T) {
	resolver, _ := appTestResolver(t)
	first := &appTestTransport{expiry: time.Hour}
	second := &appTestTransport{expiry: time.Hour}
	for _, base := range []*appTestTransport{first, second, first, second} {
		_, err := appTestSession(t.Context(), resolver, base)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"token token-1", "token token-1"}, first.tokens)
	assert.Equal(t, []string{"token token-1", "token token-1"}, second.tokens)

	inner := &appTestTransport{expiry: time.Hour}
	for range 2 {
		_, err := appTestSession(t.Context(), resolver, appTestFuncTransport(inner.RoundTrip))
		require.NoError(t, err)
	}
	assert.Len(t, inner.jwts, 2, "an uncomparable base is not cached")
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

package githubsync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

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

	kind, err := resolver.ResolveKind(context.Background(), Binding{
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

	kind, err := resolver.ResolveKind(context.Background(), Binding{
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

	rt, kind, err := resolver.transport(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)
	assert.Equal(t, CredentialKind("env"), kind)

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/example-owner/example-repo/issues", nil)
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

	kind, err := resolver.ResolveKind(context.Background(), Binding{
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

	kind, err := resolver.ResolveKind(context.Background(), Binding{
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

	rt, err := resolver.Transport(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, supplied)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/example-owner/example-repo/issues", nil)
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

	first, err := resolver.ResolveKind(context.Background(), binding)
	require.NoError(t, err)
	second, err := resolver.ResolveKind(context.Background(), binding)
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

	_, err := resolver.ResolveKind(context.Background(), Binding{
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
			req, err := http.NewRequest(http.MethodPost, tt.url, strings.NewReader(tt.body))
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
			req, err := http.NewRequest(http.MethodPost, tt.url, strings.NewReader(tt.body))
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

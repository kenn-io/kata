package githubsync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

func TestHTTPFetcherRepositoryUsesRESTBaseAndDecodesRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/repos/example-owner/example-repo", r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		writeHTTPFetcherTestResponse(t, w, `{"node_id":"R_example","id":123456789012,"full_name":"example-owner/example-repo"}`)
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	repo, err := fetcher.Repository(context.Background(), "github.com", "example-owner", "example-repo")
	require.NoError(t, err)

	assert.Equal(t, Repository{
		NodeID:   "R_example",
		ID:       123456789012,
		FullName: "example-owner/example-repo",
	}, repo)
}

func TestHTTPFetcherIssuesPaginatesAndEncodesSince(t *testing.T) {
	var server *httptest.Server
	var seenPages []string
	since := time.Date(2026, 6, 22, 10, 30, 15, 0, time.UTC)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/repos/example-owner/example-repo/issues", r.URL.Path)
		seenPages = append(seenPages, r.URL.Query().Get("page"))

		switch r.URL.Query().Get("page") {
		case "":
			query := r.URL.Query()
			assert.Equal(t, "all", query.Get("state"))
			assert.Equal(t, "updated", query.Get("sort"))
			assert.Equal(t, "asc", query.Get("direction"))
			assert.Equal(t, "100", query.Get("per_page"))
			assert.Equal(t, since.Format(time.RFC3339), query.Get("since"))
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-owner/example-repo/issues?page=2>; rel="next"`, server.URL))
			writeHTTPFetcherTestResponse(t, w, `[{"id":101,"node_id":"I_first","number":1,"title":"first"}]`)
		case "2":
			writeHTTPFetcherTestResponse(t, w, `[{"id":102,"node_id":"I_second","number":2,"title":"second"}]`)
		default:
			t.Fatalf("unexpected issues page %q", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	issues, err := fetcher.Issues(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, &since)
	require.NoError(t, err)

	require.Len(t, issues, 2)
	assert.Equal(t, "I_first", issues[0].NodeID)
	assert.Equal(t, "I_second", issues[1].NodeID)
	assert.Equal(t, []string{"", "2"}, seenPages)
}

func TestHTTPFetcherIssuesPaginationUsesOriginGuardWithCanonicalCase(t *testing.T) {
	tokenEnv := "EXAMPLE_" + "GITHUB_TOKEN"
	t.Setenv(tokenEnv, "env-token")
	var server *httptest.Server
	var seenPaths []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer env-token", r.Header.Get("Authorization"))
		seenPaths = append(seenPaths, r.URL.Path)
		switch r.URL.Query().Get("page") {
		case "":
			assert.Equal(t, "/repos/example-owner/example-repo/issues", r.URL.Path)
			w.Header().Set("Link", `<https://api.github.com/repos/Example-Owner/Example-Repo/issues?page=2>; rel="next"`)
			writeHTTPFetcherTestResponse(t, w, `[{"id":101,"node_id":"I_first","number":1,"title":"first"}]`)
		case "2":
			assert.Equal(t, "/repos/Example-Owner/Example-Repo/issues", r.URL.Path)
			writeHTTPFetcherTestResponse(t, w, `[{"id":102,"node_id":"I_second","number":2,"title":"second"}]`)
		default:
			t.Fatalf("unexpected guarded pagination page %q", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client: &http.Client{
			Transport: &rewriteHostRoundTripper{
				target: serverURL,
				next:   server.Client().Transport,
			},
		},
		CredentialResolver: NewCredentialResolver(config.GitHubSyncConfig{TokenEnv: tokenEnv}, &fakeCommandRunner{}),
	})

	issues, err := fetcher.Issues(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, nil)
	require.NoError(t, err)

	require.Len(t, issues, 2)
	assert.Equal(t, []string{"/repos/example-owner/example-repo/issues", "/repos/Example-Owner/Example-Repo/issues"}, seenPaths)
}

func TestHTTPFetcherCommentsPaginates(t *testing.T) {
	var server *httptest.Server
	var seenPages []string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/repos/example-owner/example-repo/issues/42/comments", r.URL.Path)
		seenPages = append(seenPages, r.URL.Query().Get("page"))

		switch r.URL.Query().Get("page") {
		case "":
			assert.Equal(t, "100", r.URL.Query().Get("per_page"))
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-owner/example-repo/issues/42/comments?page=2>; rel="next"`, server.URL))
			writeHTTPFetcherTestResponse(t, w, `[{"id":201,"node_id":"C_first","body":"first"}]`)
		case "2":
			writeHTTPFetcherTestResponse(t, w, `[{"id":202,"node_id":"C_second","body":"second"}]`)
		default:
			t.Fatalf("unexpected comments page %q", r.URL.Query().Get("page"))
		}
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	comments, err := fetcher.Comments(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, 42)
	require.NoError(t, err)

	require.Len(t, comments, 2)
	assert.Equal(t, "C_first", comments[0].NodeID)
	assert.Equal(t, "C_second", comments[1].NodeID)
	assert.Equal(t, []string{"", "2"}, seenPages)
}

func TestHTTPFetcherIssuesRetriesRetryableRESTStatus(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		attempts++
		switch attempts {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			writeHTTPFetcherTestResponse(t, w, `{"message":"secondary rate limit"}`)
		case 2:
			writeHTTPFetcherTestResponse(t, w, `[{"id":101,"node_id":"I_retry","number":1,"title":"retry"}]`)
		default:
			t.Fatalf("unexpected REST retry attempt %d", attempts)
		}
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	issues, err := fetcher.Issues(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, nil)
	require.NoError(t, err)

	require.Len(t, issues, 1)
	assert.Equal(t, "I_retry", issues[0].NodeID)
	assert.Equal(t, 2, attempts)
}

func TestHTTPFetcherRESTRetryTotalBudgetSpansPages(t *testing.T) {
	var server *httptest.Server
	var attempts int
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		attempts++
		switch attempts {
		case 1:
			assert.Empty(t, r.URL.Query().Get("page"))
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			writeHTTPFetcherTestResponse(t, w, `{"message":"rate limited first page"}`)
		case 2:
			assert.Empty(t, r.URL.Query().Get("page"))
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-owner/example-repo/issues?page=2>; rel="next"`, server.URL))
			writeHTTPFetcherTestResponse(t, w, `[{"id":101,"node_id":"I_first","number":1,"title":"first"}]`)
		case 3:
			assert.Equal(t, "2", r.URL.Query().Get("page"))
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			writeHTTPFetcherTestResponse(t, w, `{"message":"rate limited second page"}`)
		case 4:
			assert.Equal(t, "2", r.URL.Query().Get("page"))
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-owner/example-repo/issues?page=3>; rel="next"`, server.URL))
			writeHTTPFetcherTestResponse(t, w, `[{"id":102,"node_id":"I_second","number":2,"title":"second"}]`)
		case 5:
			assert.Equal(t, "3", r.URL.Query().Get("page"))
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			writeHTTPFetcherTestResponse(t, w, `{"message":"rate limited third page"}`)
		default:
			t.Fatalf("unexpected REST retry budget attempt %d", attempts)
		}
	}))
	defer server.Close()

	var sleeps []time.Duration
	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})
	fetcher.graphQLSleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}

	_, err := fetcher.Issues(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	}, nil)
	require.Error(t, err)

	assert.Equal(t, 5, attempts)
	assert.Equal(t, []time.Duration{30 * time.Second, 30 * time.Second}, sleeps)
	assert.Contains(t, err.Error(), "exceeds max total sleep")
}

func TestHTTPFetcherEnterpriseURLDerivation(t *testing.T) {
	allowGitHubEnterpriseHost(t, "github.example")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v3/repos/example-owner/example-repo", r.URL.Path)
		writeHTTPFetcherTestResponse(t, w, `{"node_id":"R_enterprise","id":987654321,"full_name":"example-owner/example-repo"}`)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	rewrite := &rewriteHostRoundTripper{
		target: serverURL,
		next:   server.Client().Transport,
	}

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client: &http.Client{
			Transport: rewrite,
		},
		CredentialResolver: newStaticHTTPFetcherTestResolver("test-token"),
	})

	repo, err := fetcher.Repository(context.Background(), "github.example", "example-owner", "example-repo")
	require.NoError(t, err)

	assert.Equal(t, "R_enterprise", repo.NodeID)
	require.Len(t, rewrite.seen, 1)
	assert.Equal(t, "https", rewrite.seen[0].Scheme)
	assert.Equal(t, "github.example", rewrite.seen[0].Host)
	assert.Equal(t, "/api/v3/repos/example-owner/example-repo", rewrite.seen[0].Path)
}

func TestHTTPFetcherAuthorizationDoesNotReachRedirectHost(t *testing.T) {
	var redirectAuthorization string
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectAuthorization = r.Header.Get("Authorization")
		writeHTTPFetcherTestResponse(t, w, `{"node_id":"R_redirected"}`)
	}))
	defer redirectTarget.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		http.Redirect(w, r, redirectTarget.URL+"/repos/example-owner/example-repo", http.StatusFound)
	}))
	defer origin.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              origin.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: origin.URL,
	})

	_, err := fetcher.Repository(context.Background(), "github.com", "example-owner", "example-repo")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "GitHub repository")
	assert.Contains(t, err.Error(), "302 Found")
	assert.Empty(t, redirectAuthorization)
}

func TestHTTPFetcherNon2xxErrorIncludesStatusAndRedactedBody(t *testing.T) {
	token := "g" + "hp_" + "exampleSecretMaterial"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		w.WriteHeader(http.StatusForbidden)
		writeHTTPFetcherTestResponse(t, w, fmt.Sprintf(`{"message":"bad token %s"}`, token))
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	_, err := fetcher.Repository(context.Background(), "github.com", "example-owner", "example-repo")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "403 Forbidden")
	assert.Contains(t, err.Error(), "[redacted]")
	assert.NotContains(t, err.Error(), token)
}

func TestHTTPFetcherDecodeErrorNamesGitHubResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		writeHTTPFetcherTestResponse(t, w, `{"node_id":`)
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  newStaticHTTPFetcherTestResolver("test-token"),
		RESTBaseURLOverride: server.URL,
	})

	_, err := fetcher.Repository(context.Background(), "github.com", "example-owner", "example-repo")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "decode GitHub repository")
}

func TestHTTPFetcherBindingSessionReusesCredentialTransport(t *testing.T) {
	resolver := &countingHTTPFetcherTestResolver{token: "test-token"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertHTTPFetcherHeaders(t, r)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/repos/example-owner/example-repo":
			writeHTTPFetcherTestResponse(t, w, `{"node_id":"R_example","id":123456789012,"full_name":"example-owner/example-repo"}`)
		case "/repos/example-owner/example-repo/issues":
			writeHTTPFetcherTestResponse(t, w, `[{"id":101,"node_id":"I_first","number":1,"title":"first"}]`)
		case "/repos/example-owner/example-repo/issues/1/comments":
			writeHTTPFetcherTestResponse(t, w, `[{"id":201,"node_id":"C_first","body":"first"}]`)
		case "/graphql":
			writeHTTPFetcherTestResponse(t, w, `{
				"data": {
					"repository": {
						"issues": {
							"pageInfo": {"hasNextPage": false, "endCursor": null},
							"nodes": [
								{"number": 1, "fullDatabaseId": "101", "parent": null}
							]
						}
					}
				}
			}`)
		default:
			t.Fatalf("unexpected session request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherConfig{
		Client:              server.Client(),
		CredentialResolver:  resolver,
		RESTBaseURLOverride: server.URL,
		GraphQLURLOverride:  server.URL + "/graphql",
	})
	session, err := fetcher.ForBinding(context.Background(), Binding{
		Host:  "github.com",
		Owner: "example-owner",
		Repo:  "example-repo",
	})
	require.NoError(t, err)

	_, err = session.Repository(context.Background(), "github.com", "example-owner", "example-repo")
	require.NoError(t, err)
	_, err = session.Issues(context.Background(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, nil)
	require.NoError(t, err)
	_, err = session.Comments(context.Background(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, 1)
	require.NoError(t, err)
	_, err = session.ParentData(context.Background(), Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"})
	require.NoError(t, err)

	assert.Equal(t, 1, resolver.calls)
}

func assertHTTPFetcherHeaders(t testing.TB, r *http.Request) {
	t.Helper()
	assert.Equal(t, "application/vnd.github+json", r.Header.Get("Accept"))
	assert.Equal(t, "2022-11-28", r.Header.Get("X-GitHub-Api-Version"))
}

func writeHTTPFetcherTestResponse(t testing.TB, w http.ResponseWriter, body string) {
	t.Helper()
	_, err := fmt.Fprint(w, body)
	require.NoError(t, err)
}

type staticHTTPFetcherTestResolver struct {
	token string
}

func newStaticHTTPFetcherTestResolver(token string) staticHTTPFetcherTestResolver {
	return staticHTTPFetcherTestResolver{token: token}
}

func (r staticHTTPFetcherTestResolver) Transport(_ context.Context, _ Binding, base http.RoundTripper) (http.RoundTripper, error) {
	return staticHTTPFetcherBearerTransport{token: r.token, next: base}, nil
}

type countingHTTPFetcherTestResolver struct {
	token string
	calls int
}

func (r *countingHTTPFetcherTestResolver) Transport(_ context.Context, _ Binding, base http.RoundTripper) (http.RoundTripper, error) {
	r.calls++
	return staticHTTPFetcherBearerTransport{token: r.token, next: base}, nil
}

type staticHTTPFetcherBearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t staticHTTPFetcherBearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return nextRoundTripper(t.next).RoundTrip(clone)
}

type rewriteHostRoundTripper struct {
	target *url.URL
	next   http.RoundTripper
	seen   []url.URL
}

func (t *rewriteHostRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.seen = append(t.seen, *req.URL)
	clone := req.Clone(req.Context())
	rewritten := *clone.URL
	rewritten.Scheme = t.target.Scheme
	rewritten.Host = t.target.Host
	clone.URL = &rewritten
	return nextRoundTripper(t.next).RoundTrip(clone)
}

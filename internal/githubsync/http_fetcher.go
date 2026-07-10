package githubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/config"
)

const (
	githubRESTAcceptHeader = "application/vnd.github+json"
	githubAPIVersionHeader = "2022-11-28"
	maxGitHubErrorBodySize = 8192
)

// TransportResolver resolves the scoped transport used for one GitHub binding.
type TransportResolver interface {
	Transport(ctx context.Context, binding Binding, base http.RoundTripper) (http.RoundTripper, error)
}

// HTTPFetcherConfig configures a REST-backed GitHub fetcher.
type HTTPFetcherConfig struct {
	Client              *http.Client
	CredentialResolver  TransportResolver
	RESTBaseURLOverride string
	GraphQLURLOverride  string
}

// HTTPFetcher reads GitHub repository data over the REST API.
type HTTPFetcher struct {
	client              *http.Client
	resolver            TransportResolver
	restBaseURLOverride string
	graphQLURLOverride  string
	graphQLNow          func() time.Time
	graphQLSleep        func(context.Context, time.Duration) error
	parentCapabilities  *parentCapabilityCache
}

var _ Fetcher = (*HTTPFetcher)(nil)

// NewHTTPFetcher returns a GitHub fetcher backed by net/http.
func NewHTTPFetcher(cfg HTTPFetcherConfig) *HTTPFetcher {
	client := cfg.Client
	if client == nil {
		client = http.DefaultClient
	}
	resolver := cfg.CredentialResolver
	if resolver == nil {
		resolver = NewCredentialResolver(config.GitHubSyncConfig{}, nil)
	}
	fetcher := &HTTPFetcher{
		client:              client,
		resolver:            resolver,
		restBaseURLOverride: strings.TrimSpace(cfg.RESTBaseURLOverride),
		graphQLURLOverride:  strings.TrimSpace(cfg.GraphQLURLOverride),
		graphQLNow:          time.Now,
		graphQLSleep:        sleepContext,
	}
	fetcher.parentCapabilities = newParentCapabilityCache(parentCapabilityCacheTTL, fetcher.graphQLNow)
	return fetcher
}

// Repository validates and reads a GitHub repository over REST.
func (f *HTTPFetcher) Repository(ctx context.Context, host, owner, repo string) (Repository, error) {
	binding, err := normalizeBinding(Binding{Host: host, Owner: owner, Repo: repo})
	if err != nil {
		return Repository{}, err
	}
	client, err := f.clientForBinding(ctx, binding)
	if err != nil {
		return Repository{}, err
	}
	return f.repositoryWithClient(ctx, client, binding)
}

func (f *HTTPFetcher) repositoryWithClient(ctx context.Context, client *http.Client, binding Binding) (Repository, error) {
	var out Repository
	requestURL, err := f.restEndpointURL(binding, repositoryEndpoint(binding))
	if err != nil {
		return Repository{}, err
	}
	if _, err := f.doJSON(ctx, client, gitHubJSONRequest{
		Method:   http.MethodGet,
		URL:      requestURL,
		Resource: "GitHub repository",
		Out:      &out,
	}); err != nil {
		return Repository{}, err
	}
	return out, nil
}

// Issues reads repository issues over REST, following Link rel="next" pages.
func (f *HTTPFetcher) Issues(ctx context.Context, binding Binding, since *time.Time) ([]Issue, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	requestURL, err := f.restEndpointURL(binding, issuesEndpoint(binding, since))
	if err != nil {
		return nil, err
	}
	client, err := f.clientForBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return fetchRESTPagesWithClient[Issue](ctx, f, client, binding, requestURL, "GitHub issues")
}

// Comments reads issue comments over REST, following Link rel="next" pages.
func (f *HTTPFetcher) Comments(ctx context.Context, binding Binding, issueNumber int) ([]Comment, error) {
	if issueNumber <= 0 {
		return nil, fmt.Errorf("GitHub issue number must be positive")
	}
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	requestURL, err := f.restEndpointURL(binding, commentsEndpoint(binding, issueNumber))
	if err != nil {
		return nil, err
	}
	client, err := f.clientForBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return fetchRESTPagesWithClient[Comment](ctx, f, client, binding, requestURL, "GitHub comments for issue "+strconv.Itoa(issueNumber))
}

func fetchRESTPagesWithClient[T any](ctx context.Context, f *HTTPFetcher, client *http.Client, binding Binding, firstURL, resource string) ([]T, error) {
	var out []T
	retryBudget := &gitHubRetryBudget{}
	for nextURL := firstURL; nextURL != ""; {
		currentURL := nextURL
		var page []T
		headers, err := f.doJSON(ctx, client, gitHubJSONRequest{
			Method:      http.MethodGet,
			URL:         currentURL,
			Resource:    resource,
			Out:         &page,
			RetryBudget: retryBudget,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		nextURL, err = nextGitHubLinkURL(currentURL, headers.Get("Link"))
		if err != nil {
			return nil, err
		}
		nextURL, err = canonicalGitHubPageURL(binding, nextURL)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ForBinding returns a fetcher that reuses one credential-scoped client for
// repeated reads against binding.
func (f *HTTPFetcher) ForBinding(ctx context.Context, binding Binding) (Fetcher, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	client, err := f.clientForBinding(ctx, binding)
	if err != nil {
		return nil, err
	}
	return &httpFetcherBindingSession{fetcher: f, binding: binding, client: client}, nil
}

type httpFetcherBindingSession struct {
	fetcher *HTTPFetcher
	binding Binding
	client  *http.Client
}

func (s *httpFetcherBindingSession) Repository(ctx context.Context, host, owner, repo string) (Repository, error) {
	binding, err := normalizeBinding(Binding{Host: host, Owner: owner, Repo: repo})
	if err != nil {
		return Repository{}, err
	}
	if binding != s.binding {
		return Repository{}, fmt.Errorf("GitHub fetcher session for %s/%s/%s cannot read %s/%s/%s",
			s.binding.Host, s.binding.Owner, s.binding.Repo, binding.Host, binding.Owner, binding.Repo)
	}
	return s.fetcher.repositoryWithClient(ctx, s.client, binding)
}

func (s *httpFetcherBindingSession) Issues(ctx context.Context, binding Binding, since *time.Time) ([]Issue, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	if binding != s.binding {
		return nil, fmt.Errorf("GitHub fetcher session for %s/%s/%s cannot read %s/%s/%s",
			s.binding.Host, s.binding.Owner, s.binding.Repo, binding.Host, binding.Owner, binding.Repo)
	}
	requestURL, err := s.fetcher.restEndpointURL(binding, issuesEndpoint(binding, since))
	if err != nil {
		return nil, err
	}
	return fetchRESTPagesWithClient[Issue](ctx, s.fetcher, s.client, binding, requestURL, "GitHub issues")
}

func (s *httpFetcherBindingSession) Comments(ctx context.Context, binding Binding, issueNumber int) ([]Comment, error) {
	if issueNumber <= 0 {
		return nil, fmt.Errorf("GitHub issue number must be positive")
	}
	binding, err := normalizeBinding(binding)
	if err != nil {
		return nil, err
	}
	if binding != s.binding {
		return nil, fmt.Errorf("GitHub fetcher session for %s/%s/%s cannot read %s/%s/%s",
			s.binding.Host, s.binding.Owner, s.binding.Repo, binding.Host, binding.Owner, binding.Repo)
	}
	requestURL, err := s.fetcher.restEndpointURL(binding, commentsEndpoint(binding, issueNumber))
	if err != nil {
		return nil, err
	}
	return fetchRESTPagesWithClient[Comment](ctx, s.fetcher, s.client, binding, requestURL, "GitHub comments for issue "+strconv.Itoa(issueNumber))
}

func (s *httpFetcherBindingSession) ParentData(ctx context.Context, binding Binding) (ParentData, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return ParentData{}, err
	}
	if binding != s.binding {
		return ParentData{}, fmt.Errorf("GitHub fetcher session for %s/%s/%s cannot read %s/%s/%s",
			s.binding.Host, s.binding.Owner, s.binding.Repo, binding.Host, binding.Owner, binding.Repo)
	}
	return s.fetcher.parentDataWithClient(ctx, s.client, binding)
}

func (f *HTTPFetcher) clientForBinding(ctx context.Context, binding Binding) (*http.Client, error) {
	client := *f.client
	if client.CheckRedirect == nil {
		client.CheckRedirect = noFollowGitHubRedirects
	}
	transport, err := f.resolver.Transport(ctx, binding, client.Transport)
	if err != nil {
		return nil, err
	}
	client.Transport = transport
	return &client, nil
}

func noFollowGitHubRedirects(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func (f *HTTPFetcher) restEndpointURL(binding Binding, endpoint string) (string, error) {
	rawBase := f.restBaseURLOverride
	if rawBase == "" {
		rawBase = githubRESTBaseURL(binding)
	}
	base, err := url.Parse(rawBase)
	if err != nil {
		return "", fmt.Errorf("parse GitHub REST base URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("parse GitHub REST base URL: absolute URL is required")
	}
	base.RawQuery = ""
	base.Fragment = ""
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	relative, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse GitHub REST endpoint: %w", err)
	}
	return base.ResolveReference(relative).String(), nil
}

type gitHubJSONRequest struct {
	Method      string
	URL         string
	Resource    string
	Body        io.Reader
	Out         any
	RetryBudget *gitHubRetryBudget
}

func (f *HTTPFetcher) doJSON(ctx context.Context, client *http.Client, request gitHubJSONRequest) (http.Header, error) {
	var bodyBytes []byte
	if request.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, fmt.Errorf("read %s request body: %w", request.Resource, err)
		}
	}

	budget := request.RetryBudget
	if budget == nil {
		budget = &gitHubRetryBudget{}
	}
	for attempt := 1; attempt <= gitHubMaxRetryAttempts; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, reqBody)
		if err != nil {
			return nil, fmt.Errorf("create %s request: %w", request.Resource, err)
		}
		req.Header.Set("Accept", githubRESTAcceptHeader)
		req.Header.Set("X-GitHub-Api-Version", githubAPIVersionHeader)
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := client.Do(req)
		if err != nil {
			requestErr := fmt.Errorf("request %s: %w", request.Resource, err)
			if attempt == gitHubMaxRetryAttempts {
				return nil, requestErr
			}
			if err := f.sleepForGitHubRetry(ctx, request.Resource, attempt, nil, budget); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			headers := resp.Header.Clone()
			statusErr, body, closeErr := gitHubHTTPStatusError(request.Resource, resp)
			if closeErr != nil {
				return nil, closeErr
			}
			if gitHubRESTStatusRetryable(resp.StatusCode, headers, body) && attempt < gitHubMaxRetryAttempts {
				if err := f.sleepForGitHubRetry(ctx, request.Resource, attempt, headers, budget); err != nil {
					return nil, err
				}
				continue
			}
			return nil, statusErr
		}
		headers := resp.Header.Clone()
		if err := json.NewDecoder(resp.Body).Decode(request.Out); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("decode %s: %w", request.Resource, err)
		}
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close %s response body: %w", request.Resource, err)
		}
		return headers, nil
	}
	return nil, fmt.Errorf("%s exhausted retries", request.Resource)
}

func (f *HTTPFetcher) sleepForGitHubRetry(ctx context.Context, resource string, attempt int, header http.Header, budget *gitHubRetryBudget) error {
	wait := retryWait(header, f.now())
	if wait == 0 {
		wait = time.Duration(attempt) * time.Second
	}
	if wait < 0 {
		wait = 0
	}
	if err := budget.add(wait); errors.Is(err, errGitHubRetrySingleSleepExceeded) {
		return fmt.Errorf("%s retry wait %s exceeds max single sleep %s: %w",
			resource, wait, gitHubMaxRetrySingleSleep, err)
	} else if errors.Is(err, errGitHubRetryTotalSleepExceeded) {
		return fmt.Errorf("%s retry wait total %s exceeds max total sleep %s: %w",
			resource, budget.totalSleep+wait, gitHubMaxRetryTotalSleep, err)
	}
	return f.sleep(ctx, wait)
}

func gitHubRESTStatusRetryable(status int, header http.Header, body []byte) bool {
	return graphQLHTTPStatusRetryable(status, header, body)
}

func gitHubHTTPStatusError(resource string, resp *http.Response) (error, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubErrorBodySize))
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("%s request failed: %s: read response body: %w", resource, resp.Status, err)
	}
	if closeErr != nil {
		return nil, nil, fmt.Errorf("%s request failed: %s: close response body: %w", resource, resp.Status, closeErr)
	}
	message := strings.TrimSpace(redactTokenMaterial(string(body)))
	if message == "" {
		return fmt.Errorf("%s request failed: %s", resource, resp.Status), body, nil
	}
	return fmt.Errorf("%s request failed: %s: %s", resource, resp.Status, message), body, nil
}

func nextGitHubLinkURL(currentURL, linkHeader string) (string, error) {
	next := nextGitHubLink(linkHeader)
	if next == "" {
		return "", nil
	}
	base, err := url.Parse(currentURL)
	if err != nil {
		return "", fmt.Errorf("parse current GitHub page URL: %w", err)
	}
	nextURL, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("parse GitHub next page URL: %w", err)
	}
	return base.ResolveReference(nextURL).String(), nil
}

// canonicalGitHubPageURL rewrites pagination URLs that GitHub returns in
// numeric /repositories/{id}/... form back to the /repos/{owner}/{repo}/...
// form so the credential egress guard can verify they target the bound
// repository.
func canonicalGitHubPageURL(binding Binding, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse GitHub next page URL: %w", err)
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	offset := 0
	if binding.Host != "github.com" {
		offset = 2
	}
	if len(segments) < offset+2 || segments[offset] != "repositories" {
		return raw, nil
	}
	rewritten := append([]string{}, segments[:offset]...)
	rewritten = append(rewritten, "repos", binding.Owner, binding.Repo)
	rewritten = append(rewritten, segments[offset+2:]...)
	u.Path = "/" + strings.Join(rewritten, "/")
	u.RawPath = ""
	return u.String(), nil
}

func nextGitHubLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "<") {
			continue
		}
		end := strings.Index(part, ">")
		if end <= 1 {
			continue
		}
		candidate := part[1:end]
		for _, param := range strings.Split(part[end+1:], ";") {
			param = strings.TrimSpace(param)
			if param == `rel="next"` || param == "rel=next" {
				return candidate
			}
		}
	}
	return ""
}

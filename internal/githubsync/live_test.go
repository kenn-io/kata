package githubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLiveGitHubParentGraphQLMatchesREST(t *testing.T) {
	binding, childNumber, parentNumber, token := liveGitHubParentTestConfig(t)
	fetcher := liveGitHubHTTPFetcher(token)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	data, err := fetcher.ParentData(ctx, binding)
	require.NoError(t, err)
	require.False(t, data.Unsupported, "live repository does not expose parent GraphQL fields")
	assert.True(t, data.ChildScanned(childNumber))

	parentID, ok := data.ParentID(childNumber)
	require.True(t, ok, "GraphQL parent scan did not report a parent for issue #%d", childNumber)

	restParent := liveGitHubRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d/parent", repositoryEndpoint(binding), childNumber))
	assert.Equal(t, parentNumber, restParent.Number)
	assert.Equal(t, restParent.ID, parentID)
}

func TestLiveGitHubReparentMutationBehavior(t *testing.T) {
	if os.Getenv("KATA_GITHUB_LIVE_MUTATE_REPARENT") != "1" {
		t.Skip("set KATA_GITHUB_LIVE_MUTATE_REPARENT=1 for disposable-repository reparent validation")
	}
	binding, childNumber, _, targetParentNumber, token := liveGitHubReparentTestConfig(t)
	fetcher := liveGitHubHTTPFetcher(token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	child := liveGitHubRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d", repositoryEndpoint(binding), childNumber))
	before := timeValue(child.UpdatedAt)
	require.False(t, before.IsZero(), "live child issue must have updated_at")

	originalParent, hasOriginalParent := liveGitHubOptionalRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d/parent", repositoryEndpoint(binding), childNumber))
	if hasOriginalParent && originalParent.Number == targetParentNumber {
		t.Skip("KATA_GITHUB_LIVE_TARGET_PARENT is already the child issue parent; choose a different disposable parent for mutation validation")
	}
	targetParent := liveGitHubRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d", repositoryEndpoint(binding), targetParentNumber))
	require.NotZero(t, targetParent.ID)
	require.NotZero(t, child.ID)

	reparented := false
	defer func() {
		if !reparented {
			return
		}
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer restoreCancel()
		if hasOriginalParent {
			_ = liveGitHubSubIssueRequest(restoreCtx, fetcher, binding, http.MethodPost,
				fmt.Sprintf("%s/issues/%d/sub_issues", repositoryEndpoint(binding), originalParent.Number),
				map[string]any{"sub_issue_id": child.ID, "replace_parent": true})
			return
		}
		_ = liveGitHubSubIssueRequest(restoreCtx, fetcher, binding, http.MethodDelete,
			fmt.Sprintf("%s/issues/%d/sub_issue", repositoryEndpoint(binding), targetParentNumber),
			map[string]any{"sub_issue_id": child.ID})
	}()

	require.NoError(t, liveGitHubSubIssueRequest(ctx, fetcher, binding, http.MethodPost,
		fmt.Sprintf("%s/issues/%d/sub_issues", repositoryEndpoint(binding), targetParentNumber),
		map[string]any{"sub_issue_id": child.ID, "replace_parent": true}))
	reparented = true

	reparentedParent := liveGitHubRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d/parent", repositoryEndpoint(binding), childNumber))
	assert.Equal(t, targetParentNumber, reparentedParent.Number)
	after := timeValue(liveGitHubRESTIssue(ctx, t, fetcher, binding, fmt.Sprintf("%s/issues/%d", repositoryEndpoint(binding), childNumber)).UpdatedAt)
	t.Logf("GitHub child updated_at after reparent: before=%s after=%s advanced=%t", before, after, after.After(before))
}

func TestLiveGitHubReparentConfigUsesSeparateTargetParent(t *testing.T) {
	cfg, err := parseLiveGitHubTestConfig(liveGitHubTestEnv{
		repo:         "example-owner/example-repo",
		child:        "10",
		parent:       "20",
		targetParent: "30",
		token:        "test-token",
	}, true)
	require.NoError(t, err)

	assert.Equal(t, Binding{Host: "github.com", Owner: "example-owner", Repo: "example-repo"}, cfg.binding)
	assert.Equal(t, 10, cfg.childNumber)
	assert.Equal(t, 20, cfg.parentNumber)
	assert.Equal(t, 30, cfg.targetParentNumber)
	assert.Equal(t, "test-token", cfg.token)
}

func liveGitHubParentTestConfig(t *testing.T) (Binding, int, int, string) {
	t.Helper()
	cfg, err := parseLiveGitHubTestConfig(liveGitHubTestEnvFromOS(), false)
	if errors.Is(err, errLiveGitHubMissingConfig) {
		t.Skip("set KATA_GITHUB_LIVE_REPO, KATA_GITHUB_LIVE_CHILD, KATA_GITHUB_LIVE_PARENT, and KATA_GITHUB_TOKEN or GITHUB_TOKEN for live GitHub validation")
	}
	require.NoError(t, err)
	return cfg.binding, cfg.childNumber, cfg.parentNumber, cfg.token
}

func liveGitHubReparentTestConfig(t *testing.T) (Binding, int, int, int, string) {
	t.Helper()
	cfg, err := parseLiveGitHubTestConfig(liveGitHubTestEnvFromOS(), true)
	if errors.Is(err, errLiveGitHubMissingConfig) {
		t.Skip("set KATA_GITHUB_LIVE_REPO, KATA_GITHUB_LIVE_CHILD, KATA_GITHUB_LIVE_PARENT, KATA_GITHUB_LIVE_TARGET_PARENT, and KATA_GITHUB_TOKEN or GITHUB_TOKEN for live GitHub reparent validation")
	}
	require.NoError(t, err)
	return cfg.binding, cfg.childNumber, cfg.parentNumber, cfg.targetParentNumber, cfg.token
}

var errLiveGitHubMissingConfig = errors.New("missing live GitHub test config")

type liveGitHubTestEnv struct {
	repo         string
	child        string
	parent       string
	targetParent string
	token        string
}

type liveGitHubTestConfig struct {
	binding            Binding
	childNumber        int
	parentNumber       int
	targetParentNumber int
	token              string
}

func liveGitHubTestEnvFromOS() liveGitHubTestEnv {
	token := strings.TrimSpace(os.Getenv("KATA_GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	return liveGitHubTestEnv{
		repo:         strings.TrimSpace(os.Getenv("KATA_GITHUB_LIVE_REPO")),
		child:        strings.TrimSpace(os.Getenv("KATA_GITHUB_LIVE_CHILD")),
		parent:       strings.TrimSpace(os.Getenv("KATA_GITHUB_LIVE_PARENT")),
		targetParent: strings.TrimSpace(os.Getenv("KATA_GITHUB_LIVE_TARGET_PARENT")),
		token:        token,
	}
}

func parseLiveGitHubTestConfig(env liveGitHubTestEnv, requireTargetParent bool) (liveGitHubTestConfig, error) {
	env.repo = strings.TrimSpace(env.repo)
	env.child = strings.TrimSpace(env.child)
	env.parent = strings.TrimSpace(env.parent)
	env.targetParent = strings.TrimSpace(env.targetParent)
	env.token = strings.TrimSpace(env.token)
	if env.repo == "" || env.child == "" || env.parent == "" || env.token == "" {
		return liveGitHubTestConfig{}, errLiveGitHubMissingConfig
	}
	if requireTargetParent && env.targetParent == "" {
		return liveGitHubTestConfig{}, errLiveGitHubMissingConfig
	}
	owner, name, ok := strings.Cut(env.repo, "/")
	if !ok || strings.TrimSpace(owner) == "" || strings.TrimSpace(name) == "" {
		return liveGitHubTestConfig{}, fmt.Errorf("KATA_GITHUB_LIVE_REPO must be owner/repo, got %q", env.repo)
	}
	childNumber, err := strconv.Atoi(env.child)
	if err != nil {
		return liveGitHubTestConfig{}, fmt.Errorf("parse KATA_GITHUB_LIVE_CHILD: %w", err)
	}
	parentNumber, err := strconv.Atoi(env.parent)
	if err != nil {
		return liveGitHubTestConfig{}, fmt.Errorf("parse KATA_GITHUB_LIVE_PARENT: %w", err)
	}
	cfg := liveGitHubTestConfig{
		binding:      Binding{Host: "github.com", Owner: owner, Repo: name},
		childNumber:  childNumber,
		parentNumber: parentNumber,
		token:        env.token,
	}
	if requireTargetParent {
		targetParentNumber, err := strconv.Atoi(env.targetParent)
		if err != nil {
			return liveGitHubTestConfig{}, fmt.Errorf("parse KATA_GITHUB_LIVE_TARGET_PARENT: %w", err)
		}
		cfg.targetParentNumber = targetParentNumber
	}
	return cfg, nil
}

func liveGitHubHTTPFetcher(token string) *HTTPFetcher {
	return NewHTTPFetcher(HTTPFetcherConfig{CredentialResolver: liveTokenResolver{token: token}})
}

type liveTokenResolver struct {
	token string
}

func (r liveTokenResolver) Transport(_ context.Context, binding Binding, base http.RoundTripper) (http.RoundTripper, error) {
	return newOriginGuardTransport(binding, &staticBearerTransport{token: r.token, next: base}), nil
}

func liveGitHubRESTIssue(ctx context.Context, t *testing.T, fetcher *HTTPFetcher, binding Binding, endpoint string) Issue {
	t.Helper()
	var issue Issue
	require.NoError(t, liveGitHubRESTJSON(ctx, fetcher, binding, http.MethodGet, endpoint, nil, &issue))
	return issue
}

func liveGitHubOptionalRESTIssue(ctx context.Context, t *testing.T, fetcher *HTTPFetcher, binding Binding, endpoint string) (Issue, bool) {
	t.Helper()
	var issue Issue
	err := liveGitHubRESTJSON(ctx, fetcher, binding, http.MethodGet, endpoint, nil, &issue)
	if err == nil {
		return issue, true
	}
	if strings.Contains(err.Error(), "404") {
		return Issue{}, false
	}
	require.NoError(t, err)
	return Issue{}, false
}

func liveGitHubSubIssueRequest(ctx context.Context, fetcher *HTTPFetcher, binding Binding, method, endpoint string, body map[string]any) error {
	var out Issue
	return liveGitHubRESTJSON(ctx, fetcher, binding, method, endpoint, body, &out)
}

func liveGitHubRESTJSON(ctx context.Context, fetcher *HTTPFetcher, binding Binding, method, endpoint string, body any, out any) error {
	requestURL, err := fetcher.restEndpointURL(binding, endpoint)
	if err != nil {
		return err
	}
	client, err := fetcher.clientForBinding(ctx, binding)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	headers, err := fetcher.doJSON(ctx, client, gitHubJSONRequest{
		Method:   method,
		URL:      requestURL,
		Resource: "live GitHub REST validation",
		Body:     reader,
		Out:      out,
	})
	if err != nil {
		return err
	}
	_ = headers
	return nil
}

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
	"sync"
	"time"
)

const (
	parentGraphQLResource     = "GitHub parent GraphQL data"
	parentCapabilityCacheTTL  = time.Hour
	gitHubMaxRetryAttempts    = 3
	gitHubMaxRetrySingleSleep = 30 * time.Second
	gitHubMaxRetryTotalSleep  = 60 * time.Second
)

const parentGraphQLQuery = `
query($owner: String!, $repo: String!, $after: String) {
  repository(owner: $owner, name: $repo) {
    issues(first: 100, after: $after, states: [OPEN, CLOSED]) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number
        fullDatabaseId
        parent { number fullDatabaseId }
      }
    }
  }
}`

var errParentFeatureUnsupported = errors.New("GitHub parent GraphQL feature unsupported")

// ParentData fetches child->parent REST database IDs through GitHub GraphQL.
func (f *HTTPFetcher) ParentData(ctx context.Context, binding Binding) (ParentData, error) {
	binding, err := normalizeBinding(binding)
	if err != nil {
		return ParentData{}, err
	}
	client, err := f.clientForBinding(ctx, binding)
	if err != nil {
		return ParentData{}, err
	}
	return f.parentDataWithClient(ctx, client, binding)
}

func (f *HTTPFetcher) parentDataWithClient(ctx context.Context, client *http.Client, binding Binding) (ParentData, error) {
	cache := f.parentCapabilityCache()
	if cache.featureUnsupported(binding.Host) {
		return ParentData{Unsupported: true}, nil
	}
	requestURL, err := f.graphQLEndpointURL(binding)
	if err != nil {
		return ParentData{}, err
	}
	data := ParentData{
		ParentByChild:   map[int]int64{},
		ScannedChildren: map[int]struct{}{},
		ChildIDByNumber: map[int]int64{},
		Authoritative:   true,
	}
	var after *string
	retryBudget := &gitHubRetryBudget{}
	for {
		page, err := f.fetchParentGraphQLPage(ctx, client, requestURL, binding, after, retryBudget)
		if err != nil {
			if errors.Is(err, errParentFeatureUnsupported) {
				cache.markUnsupported(binding.Host)
				return ParentData{Unsupported: true}, nil
			}
			return ParentData{}, err
		}
		for _, node := range page.Nodes {
			if node.Number <= 0 {
				continue
			}
			data.ScannedChildren[node.Number] = struct{}{}
			if node.FullDatabaseID <= 0 {
				return ParentData{}, fmt.Errorf("%s child issue %d missing fullDatabaseId",
					parentGraphQLResource, node.Number)
			}
			data.ChildIDByNumber[node.Number] = int64(node.FullDatabaseID)
			if node.Parent != nil {
				if node.Parent.FullDatabaseID <= 0 {
					return ParentData{}, fmt.Errorf("%s parent issue %d for child issue %d missing fullDatabaseId",
						parentGraphQLResource, node.Parent.Number, node.Number)
				}
				data.ParentByChild[node.Number] = int64(node.Parent.FullDatabaseID)
			}
		}
		if !page.PageInfo.HasNextPage {
			return data, nil
		}
		if page.PageInfo.EndCursor == "" {
			return ParentData{}, fmt.Errorf("%s pagination missing end cursor", parentGraphQLResource)
		}
		if after != nil && page.PageInfo.EndCursor == *after {
			return ParentData{}, fmt.Errorf("%s pagination cursor did not advance from %q", parentGraphQLResource, page.PageInfo.EndCursor)
		}
		after = &page.PageInfo.EndCursor
	}
}

func (f *HTTPFetcher) fetchParentGraphQLPage(ctx context.Context, client *http.Client, requestURL string, binding Binding, after *string, budget *gitHubRetryBudget) (parentGraphQLIssues, error) {
	for attempt := 1; attempt <= gitHubMaxRetryAttempts; attempt++ {
		page, retry, err := f.fetchParentGraphQLPageOnce(ctx, client, requestURL, binding, after)
		if err == nil {
			return page, nil
		}
		if errors.Is(err, errParentFeatureUnsupported) {
			return parentGraphQLIssues{}, err
		}
		if !retry.Retryable || attempt == gitHubMaxRetryAttempts {
			return parentGraphQLIssues{}, err
		}
		wait := retry.Wait
		if wait == 0 {
			wait = time.Duration(attempt) * time.Second
		}
		if wait < 0 {
			wait = 0
		}
		if err := budget.add(wait); errors.Is(err, errGitHubRetrySingleSleepExceeded) {
			return parentGraphQLIssues{}, fmt.Errorf("%s retry wait %s exceeds max single sleep %s: %w",
				parentGraphQLResource, wait, gitHubMaxRetrySingleSleep, err)
		} else if errors.Is(err, errGitHubRetryTotalSleepExceeded) {
			return parentGraphQLIssues{}, fmt.Errorf("%s retry wait total %s exceeds max total sleep %s: %w",
				parentGraphQLResource, budget.totalSleep+wait, gitHubMaxRetryTotalSleep, err)
		}
		if err := f.sleep(ctx, wait); err != nil {
			return parentGraphQLIssues{}, err
		}
	}
	return parentGraphQLIssues{}, fmt.Errorf("%s exhausted retries", parentGraphQLResource)
}

func (f *HTTPFetcher) fetchParentGraphQLPageOnce(ctx context.Context, client *http.Client, requestURL string, binding Binding, after *string) (parentGraphQLIssues, gitHubRetry, error) {
	body, err := json.Marshal(parentGraphQLRequest{
		Query: parentGraphQLQuery,
		Variables: map[string]any{
			"owner": binding.Owner,
			"repo":  binding.Repo,
			"after": after,
		},
	})
	if err != nil {
		return parentGraphQLIssues{}, gitHubRetry{}, fmt.Errorf("encode %s request: %w", parentGraphQLResource, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return parentGraphQLIssues{}, gitHubRetry{}, fmt.Errorf("create %s request: %w", parentGraphQLResource, err)
	}
	req.Header.Set("Accept", githubRESTAcceptHeader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersionHeader)

	resp, err := client.Do(req)
	if err != nil {
		return parentGraphQLIssues{}, gitHubRetry{Retryable: true}, fmt.Errorf("request %s: %w", parentGraphQLResource, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxGitHubErrorBodySize))
		if readErr != nil {
			return parentGraphQLIssues{}, gitHubRetry{}, fmt.Errorf("%s request failed: %s: read response body: %w",
				parentGraphQLResource, resp.Status, readErr)
		}
		statusErr := gitHubGraphQLStatusError(resp.Status, body)
		if graphQLHTTPStatusRetryable(resp.StatusCode, resp.Header, body) {
			return parentGraphQLIssues{}, gitHubRetry{
				Retryable: true,
				Wait:      retryWait(resp.Header, f.now()),
			}, statusErr
		}
		return parentGraphQLIssues{}, gitHubRetry{}, statusErr
	}

	var out parentGraphQLResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return parentGraphQLIssues{}, gitHubRetry{}, fmt.Errorf("decode %s: %w", parentGraphQLResource, err)
	}
	if len(out.Errors) > 0 {
		if graphQLErrorsRateLimited(out.Errors) {
			return parentGraphQLIssues{}, gitHubRetry{
				Retryable: true,
				Wait:      retryWait(resp.Header, f.now()),
			}, graphQLErrorsError(out.Errors)
		}
		if graphQLErrorsFeatureUnsupported(out.Errors) {
			return parentGraphQLIssues{}, gitHubRetry{}, errParentFeatureUnsupported
		}
		return parentGraphQLIssues{}, gitHubRetry{}, graphQLErrorsError(out.Errors)
	}
	if out.Data == nil || out.Data.Repository == nil {
		return parentGraphQLIssues{}, gitHubRetry{}, fmt.Errorf("%s response missing repository data", parentGraphQLResource)
	}
	return out.Data.Repository.Issues, gitHubRetry{}, nil
}

func (f *HTTPFetcher) graphQLEndpointURL(binding Binding) (string, error) {
	rawURL := f.graphQLURLOverride
	if rawURL == "" {
		rawURL = githubGraphQLURL(binding)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse GitHub GraphQL URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("parse GitHub GraphQL URL: absolute URL is required")
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func githubGraphQLURL(binding Binding) string {
	if binding.Host == "github.com" {
		return "https://api.github.com/graphql"
	}
	return "https://" + binding.Host + "/api/graphql"
}

func graphQLHTTPStatusRetryable(status int, header http.Header, body []byte) bool {
	if status == http.StatusTooManyRequests || (status >= 500 && status <= 599) {
		return true
	}
	if status == http.StatusForbidden && strings.TrimSpace(header.Get("X-RateLimit-Remaining")) == "0" {
		return true
	}
	trimmed := strings.TrimSpace(string(body))
	return status == http.StatusForbidden && strings.Contains(strings.ToLower(trimmed), "rate limit")
}

func gitHubGraphQLStatusError(status string, body []byte) error {
	message := strings.TrimSpace(redactTokenMaterial(string(body)))
	if message == "" {
		return fmt.Errorf("%s request failed: %s", parentGraphQLResource, status)
	}
	return fmt.Errorf("%s request failed: %s: %s", parentGraphQLResource, status, message)
}

func graphQLErrorsRateLimited(errors []parentGraphQLError) bool {
	for _, graphQLError := range errors {
		if graphQLError.rateLimited() {
			return true
		}
	}
	return false
}

func graphQLErrorsFeatureUnsupported(errors []parentGraphQLError) bool {
	if len(errors) == 0 {
		return false
	}
	for _, graphQLError := range errors {
		if !graphQLError.featureUnsupported() {
			return false
		}
	}
	return true
}

func graphQLErrorsError(errors []parentGraphQLError) error {
	parts := make([]string, 0, len(errors))
	for _, graphQLError := range errors {
		message := strings.TrimSpace(redactTokenMaterial(graphQLError.Message))
		if message == "" {
			message = "GraphQL error"
		}
		if graphQLError.Type != "" {
			message = graphQLError.Type + ": " + message
		}
		parts = append(parts, message)
	}
	return fmt.Errorf("%s returned errors: %s", parentGraphQLResource, strings.Join(parts, "; "))
}

func retryWait(header http.Header, now time.Time) time.Duration {
	if wait, ok := retryAfterWait(header.Get("Retry-After"), now); ok {
		return wait
	}
	if wait, ok := rateLimitResetWait(header.Get("X-RateLimit-Reset"), now); ok {
		return wait
	}
	return 0
}

func retryAfterWait(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		const maxDurationSeconds = int64(1<<63-1) / int64(time.Second)
		if seconds > maxDurationSeconds {
			return time.Duration(1<<63 - 1), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	resetAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	wait := resetAt.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

func rateLimitResetWait(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false
	}
	wait := time.Unix(seconds, 0).Sub(now)
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

func (f *HTTPFetcher) now() time.Time {
	return f.graphQLNow()
}

func (f *HTTPFetcher) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	return f.graphQLSleep(ctx, d)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *HTTPFetcher) parentCapabilityCache() *parentCapabilityCache {
	return f.parentCapabilities
}

type parentGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type parentGraphQLResponse struct {
	Data   *parentGraphQLData   `json:"data"`
	Errors []parentGraphQLError `json:"errors"`
}

type parentGraphQLData struct {
	Repository *parentGraphQLRepository `json:"repository"`
}

type parentGraphQLRepository struct {
	Issues parentGraphQLIssues `json:"issues"`
}

type parentGraphQLIssues struct {
	PageInfo parentGraphQLPageInfo `json:"pageInfo"`
	Nodes    []parentGraphQLNode   `json:"nodes"`
}

type parentGraphQLPageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type parentGraphQLNode struct {
	Number         int                  `json:"number"`
	FullDatabaseID graphQLBigInt        `json:"fullDatabaseId"`
	Parent         *parentGraphQLParent `json:"parent"`
}

type parentGraphQLParent struct {
	Number         int           `json:"number"`
	FullDatabaseID graphQLBigInt `json:"fullDatabaseId"`
}

type graphQLBigInt int64

func (v *graphQLBigInt) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		*v = 0
		return nil
	}
	var text string
	if data[0] == '"' {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
	} else {
		text = string(data)
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("parse GraphQL BigInt %q: %w", text, err)
	}
	*v = graphQLBigInt(parsed)
	return nil
}

type parentGraphQLError struct {
	Type       string         `json:"type"`
	Message    string         `json:"message"`
	Path       []any          `json:"path"`
	Extensions map[string]any `json:"extensions"`
}

func (e parentGraphQLError) rateLimited() bool {
	return graphQLErrorHasClass(e, "ratelimited", "ratelimit")
}

func (e parentGraphQLError) featureUnsupported() bool {
	if e.hasUndefinedFieldClass() {
		return true
	}
	if graphQLErrorHasExactClass(e, "graphqlvalidationfailed") && e.referencesUnsupportedField() {
		return true
	}
	if !graphQLMessageIsUnsupportedFieldValidation(e.Message) {
		return false
	}
	if !e.hasClassMetadata() {
		return true
	}
	return graphQLMessageIsUnsupportedFieldValidation(e.Message) && graphQLErrorHasExactClass(e,
		"graphqlvalidationfailed",
	)
}

func (e parentGraphQLError) hasUndefinedFieldClass() bool {
	return graphQLErrorHasExactClass(e,
		"undefinedfield",
		"unknownfield",
		"fieldundefined",
	)
}

func (e parentGraphQLError) referencesUnsupportedField() bool {
	for _, element := range e.Path {
		if field, ok := element.(string); ok && isUnsupportedParentGraphQLField(field) {
			return true
		}
	}
	for _, key := range []string{"fieldName", "field", "name"} {
		if raw, ok := e.Extensions[key]; ok {
			if field, ok := raw.(string); ok && isUnsupportedParentGraphQLField(field) {
				return true
			}
		}
	}
	return false
}

func isUnsupportedParentGraphQLField(field string) bool {
	switch normalizeGraphQLErrorClass(field) {
	case "parent", "databaseid", "fulldatabaseid":
		return true
	default:
		return false
	}
}

func graphQLMessageIsUnsupportedFieldValidation(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if message == "" {
		return false
	}
	for _, field := range []string{"parent", "databaseid", "fulldatabaseid"} {
		if strings.Contains(message, "field '"+field+"' doesn't exist") ||
			strings.Contains(message, `field "`+field+`" doesn't exist`) ||
			strings.Contains(message, "cannot query field '"+field+"' on type 'issue'") ||
			strings.Contains(message, `cannot query field "`+field+`" on type "issue"`) {
			return true
		}
	}
	return false
}

func (e parentGraphQLError) hasClassMetadata() bool {
	if strings.TrimSpace(e.Type) != "" {
		return true
	}
	for _, key := range []string{"code", "type", "classification"} {
		if raw, ok := e.Extensions[key]; ok {
			if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func graphQLErrorHasClass(e parentGraphQLError, classes ...string) bool {
	for _, value := range e.classValues() {
		normalized := normalizeGraphQLErrorClass(value)
		for _, class := range classes {
			if normalized == class || strings.Contains(normalized, class) {
				return true
			}
		}
	}
	return false
}

func graphQLErrorHasExactClass(e parentGraphQLError, classes ...string) bool {
	for _, value := range e.classValues() {
		normalized := normalizeGraphQLErrorClass(value)
		for _, class := range classes {
			if normalized == class {
				return true
			}
		}
	}
	return false
}

func (e parentGraphQLError) classValues() []string {
	values := []string{e.Type}
	for _, key := range []string{"code", "type", "classification"} {
		if raw, ok := e.Extensions[key]; ok {
			switch value := raw.(type) {
			case string:
				values = append(values, value)
			}
		}
	}
	return values
}

func normalizeGraphQLErrorClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, value)
}

type gitHubRetry struct {
	Retryable bool
	Wait      time.Duration
}

var (
	errGitHubRetrySingleSleepExceeded = errors.New("single retry sleep budget exceeded")
	errGitHubRetryTotalSleepExceeded  = errors.New("total retry sleep budget exceeded")
)

type gitHubRetryBudget struct {
	totalSleep time.Duration
}

func (b *gitHubRetryBudget) add(wait time.Duration) error {
	if wait > gitHubMaxRetrySingleSleep {
		return errGitHubRetrySingleSleepExceeded
	}
	if b.totalSleep+wait > gitHubMaxRetryTotalSleep {
		return errGitHubRetryTotalSleepExceeded
	}
	b.totalSleep += wait
	return nil
}

type parentCapabilityCache struct {
	mu      sync.Mutex
	entries map[string]capabilityEntry
	ttl     time.Duration
	now     func() time.Time
}

type capabilityEntry struct {
	unsupported bool
	expiresAt   time.Time
}

func newParentCapabilityCache(ttl time.Duration, now func() time.Time) *parentCapabilityCache {
	if ttl <= 0 {
		ttl = parentCapabilityCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &parentCapabilityCache{
		entries: map[string]capabilityEntry{},
		ttl:     ttl,
		now:     now,
	}
}

func (c *parentCapabilityCache) featureUnsupported(host string) bool {
	if c == nil {
		return false
	}
	host = normalizeCapabilityHost(host)
	if host == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[host]
	if !ok || !entry.unsupported {
		return false
	}
	if !c.now().Before(entry.expiresAt) {
		delete(c.entries, host)
		return false
	}
	return true
}

func (c *parentCapabilityCache) markUnsupported(host string) {
	if c == nil {
		return
	}
	host = normalizeCapabilityHost(host)
	if host == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[host] = capabilityEntry{
		unsupported: true,
		expiresAt:   c.now().Add(c.ttl),
	}
}

func normalizeCapabilityHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

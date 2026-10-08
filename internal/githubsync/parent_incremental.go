package githubsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	parentQueryBatchSize = 100
	parentEventsPageSize = 100
)

// Integer literals are generated internally; matching the complete reconstructed
// query keeps the credential guard restricted to this read-only operation.
var parentIssueAlias = regexp.MustCompile(`i[0-9]+:issue\(number:([0-9]+)\)`)

func targetedParentGraphQLQuery(numbers []int) string {
	var query strings.Builder
	query.WriteString("query($owner: String!, $repo: String!) { repository(owner: $owner, name: $repo) {")
	for i, number := range numbers {
		_, _ = fmt.Fprintf(&query, " i%d: issue(number: %d) { number fullDatabaseId parent { number fullDatabaseId } }", i, number)
	}
	query.WriteString(" } }")
	return query.String()
}

func matchesTargetedParentQuery(query string) bool {
	compact := compactGraphQLQuery(query)
	matches := parentIssueAlias.FindAllStringSubmatch(compact, -1)
	if len(matches) == 0 || len(matches) > parentQueryBatchSize {
		return false
	}
	numbers := make([]int, len(matches))
	for i, match := range matches {
		n, err := strconv.Atoi(match[1])
		if err != nil || n <= 0 {
			return false
		}
		numbers[i] = n
	}
	return compact == compactGraphQLQuery(targetedParentGraphQLQuery(numbers))
}

// parentErrorsWithoutMissingChildren drops NOT_FOUND errors for aliases that
// resolved to null and returns those alias indexes as missing children.
func parentErrorsWithoutMissingChildren(errs []parentGraphQLError, data *parentGraphQLData, count int) ([]parentGraphQLError, map[int]bool) {
	remaining := make([]parentGraphQLError, 0, len(errs))
	missing := map[int]bool{}
	for _, e := range errs {
		skipped := false
		if e.Type == "NOT_FOUND" && data != nil && len(e.Path) == 2 && e.Path[0] == "repository" {
			for i := range count {
				alias := fmt.Sprintf("i%d", i)
				raw, ok := data.Repository[alias]
				if e.Path[1] == alias && ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					missing[i] = true
					skipped = true
					break
				}
			}
		}
		if !skipped {
			remaining = append(remaining, e)
		}
	}
	return remaining, missing
}

func (f *HTTPFetcher) incrementalParentData(ctx context.Context, client *http.Client, binding Binding, request ParentRequest) (ParentData, error) {
	budget := &gitHubRetryBudget{}
	numbers, covered, err := f.parentEventChildren(ctx, client, binding, *request.Since, budget)
	if err != nil {
		return ParentData{}, err
	}
	if !covered {
		return f.fullParentData(ctx, client, binding)
	}
	request.IssueNumbers = append(numbers, request.IssueNumbers...)
	return f.selectedParentData(ctx, client, binding, request, ParentScanIncremental, budget)
}

// selectedParentData checks explicit children without reading historical events.
// The caller supplies whether this covers a bootstrap or an incremental pass.
func (f *HTTPFetcher) selectedParentData(ctx context.Context, client *http.Client, binding Binding, request ParentRequest, scan ParentScan, budget *gitHubRetryBudget) (ParentData, error) {
	numbers := slices.Clone(request.IssueNumbers)
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	numbers = slices.DeleteFunc(numbers, func(n int) bool { return n <= 0 })
	data := ParentData{Scan: scan, ScannedChildIDs: map[int]int64{}, ParentByChild: map[int]int64{}}
	reportProgress(ctx, "parents", 0, len(numbers))
	if len(numbers) == 0 {
		return data, nil
	}
	requestURL, err := f.graphQLEndpointURL(binding)
	if err != nil {
		return ParentData{}, err
	}
	// Check the selected children before listing sub-issues, so a host without
	// parent support is detected before it can reject the sub-issue endpoint.
	err = f.checkParentBatches(ctx, client, binding, requestURL, budget, numbers, 0, &data)
	if err == nil && len(request.ChildrenOf) > 0 {
		err = f.checkChildrenOf(ctx, client, binding, requestURL, budget, request.ChildrenOf, numbers, &data)
	}
	if errors.Is(err, errParentFeatureUnsupported) {
		f.parentCapabilityCache().markUnsupported(binding.Host)
		return ParentData{Scan: ParentScanUnsupported}, nil
	}
	if err != nil {
		return ParentData{}, err
	}
	return data, nil
}

// checkChildrenOf checks the same-repository sub-issues of parents that were
// not already selected.
func (f *HTTPFetcher) checkChildrenOf(ctx context.Context, client *http.Client, binding Binding, requestURL string, budget *gitHubRetryBudget, parents, selected []int, data *ParentData) error {
	children, err := f.sameRepositorySubIssues(ctx, client, binding, parents)
	if err != nil {
		return err
	}
	slices.Sort(children)
	children = slices.Compact(children)
	children = slices.DeleteFunc(children, func(n int) bool {
		_, found := slices.BinarySearch(selected, n)
		return n <= 0 || found
	})
	if len(children) == 0 {
		return nil
	}
	return f.checkParentBatches(ctx, client, binding, requestURL, budget, children, len(selected), data)
}

// checkParentBatches reads the current parent of each number in GraphQL batches
// and records the results in data. done is the count already reported.
func (f *HTTPFetcher) checkParentBatches(ctx context.Context, client *http.Client, binding Binding, requestURL string, budget *gitHubRetryBudget, numbers []int, done int, data *ParentData) error {
	for start := 0; start < len(numbers); start += parentQueryBatchSize {
		end := min(start+parentQueryBatchSize, len(numbers))
		page, err := f.fetchParentGraphQLPage(ctx, client, requestURL, binding, nil, budget, numbers[start:end])
		if err != nil {
			return err
		}
		for _, node := range page.Nodes {
			if node.FullDatabaseID <= 0 {
				return fmt.Errorf("%s child issue %d missing fullDatabaseId", parentGraphQLResource, node.Number)
			}
			data.ScannedChildIDs[node.Number] = int64(node.FullDatabaseID)
			if node.Parent != nil {
				if node.Parent.FullDatabaseID <= 0 {
					return fmt.Errorf("%s parent for child issue %d missing fullDatabaseId", parentGraphQLResource, node.Number)
				}
				data.ParentByChild[node.Number] = int64(node.Parent.FullDatabaseID)
			}
		}
		reportProgress(ctx, "parents", done+end, done+len(numbers))
	}
	return nil
}

type subIssue struct {
	Number     int `json:"number"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// sameRepositorySubIssues lists the sub-issues of each parent. It keeps only
// children in the bound repository, because a child number from another
// repository would name a different issue here.
func (f *HTTPFetcher) sameRepositorySubIssues(ctx context.Context, client *http.Client, binding Binding, parents []int) ([]int, error) {
	fullName := binding.Owner + "/" + binding.Repo
	var children []int
	for _, parent := range parents {
		requestURL, err := f.restEndpointURL(binding, repositoryEndpoint(binding)+"/issues/"+strconv.Itoa(parent)+"/sub_issues?per_page=100")
		if err != nil {
			return nil, err
		}
		subIssues, err := fetchRESTPagesWithClient[subIssue](ctx, f, client, binding, requestURL, "GitHub sub-issues for issue "+strconv.Itoa(parent), "")
		if err != nil {
			return nil, err
		}
		for _, sub := range subIssues {
			if sub.Repository != nil && strings.EqualFold(sub.Repository.FullName, fullName) {
				children = append(children, sub.Number)
			}
		}
	}
	return children, nil
}

type parentIssueEvent struct {
	Event     string     `json:"event"`
	CreatedAt *time.Time `json:"created_at"`
	Issue     *struct {
		Number int `json:"number"`
	} `json:"issue"`
}

// parentEventChildren returns children named by parent events at or after since.
// covered is false when the feed ends on a full page without reaching since:
// GitHub caps this feed at 30,000 events and omits the next link on that page.
func (f *HTTPFetcher) parentEventChildren(ctx context.Context, client *http.Client, binding Binding, since time.Time, budget *gitHubRetryBudget) ([]int, bool, error) {
	next, err := f.restEndpointURL(binding, repositoryEndpoint(binding)+"/issues/events?per_page="+strconv.Itoa(parentEventsPageSize))
	if err != nil {
		return nil, false, err
	}
	var numbers []int
	visited := map[string]bool{}
	lastPageFull := false
	for next != "" {
		if visited[next] {
			return nil, false, fmt.Errorf("GitHub parent events pagination did not advance")
		}
		visited[next] = true
		current := next
		var page []parentIssueEvent
		headers, err := f.doJSON(ctx, client, gitHubJSONRequest{Method: http.MethodGet, URL: current, Resource: "GitHub parent events", Out: &page, RetryBudget: budget, Binding: binding})
		if err != nil {
			return nil, false, err
		}
		lastPageFull = len(page) >= parentEventsPageSize
		crossed := false
		// GitHub's repository issue-event feed is observed newest first. Validate
		// the whole page before stopping; equality must continue onto the next page.
		for _, event := range page {
			if event.CreatedAt == nil {
				return nil, false, fmt.Errorf("GitHub parent event missing created_at")
			}
			if event.CreatedAt.Before(since) {
				crossed = true
				continue
			}
			if event.Event == "parent_issue_added" || event.Event == "parent_issue_removed" {
				if event.Issue == nil || event.Issue.Number <= 0 {
					return nil, false, fmt.Errorf("GitHub parent event missing child issue number")
				}
				numbers = append(numbers, event.Issue.Number)
			}
		}
		if crossed {
			return numbers, true, nil
		}
		next, err = nextGitHubLinkURL(current, headers.Get("Link"))
		if err != nil {
			return nil, false, err
		}
		next, err = canonicalGitHubPageURL(binding, next)
		if err != nil {
			return nil, false, err
		}
	}
	return numbers, !lastPageFull, nil
}

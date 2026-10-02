package githubsync

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const parentQueryBatchSize = 100

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

func parentErrorsWithoutMissingChildren(errs []parentGraphQLError, data *parentGraphQLData, count int) []parentGraphQLError {
	remaining := make([]parentGraphQLError, 0, len(errs))
	for _, e := range errs {
		skipped := false
		if e.Type == "NOT_FOUND" && data != nil && len(e.Path) == 2 && e.Path[0] == "repository" {
			for i := range count {
				alias := fmt.Sprintf("i%d", i)
				raw, ok := data.Repository[alias]
				if e.Path[1] == alias && ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					if data.missingChildren == nil {
						data.missingChildren = map[int]bool{}
					}
					data.missingChildren[i] = true
					skipped = true
					break
				}
			}
		}
		if !skipped {
			remaining = append(remaining, e)
		}
	}
	return remaining
}

func missingParentChild(data *parentGraphQLData, index int) bool { return data.missingChildren[index] }

func (f *HTTPFetcher) incrementalParentData(ctx context.Context, client *http.Client, binding Binding, request ParentRequest) (ParentData, error) {
	budget := &gitHubRetryBudget{}
	numbers, err := f.parentEventChildren(ctx, client, binding, *request.Since, budget)
	if err != nil {
		return ParentData{}, err
	}
	numbers = append(numbers, request.IssueNumbers...)
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	numbers = slices.DeleteFunc(numbers, func(n int) bool { return n <= 0 })
	data := ParentData{Scan: ParentScanIncremental, ScannedChildIDs: map[int]int64{}, ParentByChild: map[int]int64{}}
	reportProgress(ctx, "parents", 0, len(numbers))
	if len(numbers) == 0 {
		return data, nil
	}
	requestURL, err := f.graphQLEndpointURL(binding)
	if err != nil {
		return ParentData{}, err
	}
	for start := 0; start < len(numbers); start += parentQueryBatchSize {
		end := min(start+parentQueryBatchSize, len(numbers))
		page, err := f.fetchParentGraphQLPage(ctx, client, requestURL, binding, nil, budget, numbers[start:end])
		if err != nil {
			if err == errParentFeatureUnsupported {
				f.parentCapabilityCache().markUnsupported(binding.Host)
				return ParentData{Scan: ParentScanUnsupported}, nil
			}
			return ParentData{}, err
		}
		for _, node := range page.Nodes {
			if node.FullDatabaseID <= 0 {
				return ParentData{}, fmt.Errorf("%s child issue %d missing fullDatabaseId", parentGraphQLResource, node.Number)
			}
			data.ScannedChildIDs[node.Number] = int64(node.FullDatabaseID)
			if node.Parent != nil {
				if node.Parent.FullDatabaseID <= 0 {
					return ParentData{}, fmt.Errorf("%s parent for child issue %d missing fullDatabaseId", parentGraphQLResource, node.Number)
				}
				data.ParentByChild[node.Number] = int64(node.Parent.FullDatabaseID)
			}
		}
		reportProgress(ctx, "parents", end, len(numbers))
	}
	return data, nil
}

type parentIssueEvent struct {
	Event     string     `json:"event"`
	CreatedAt *time.Time `json:"created_at"`
	Issue     *struct {
		Number int `json:"number"`
	} `json:"issue"`
}

func (f *HTTPFetcher) parentEventChildren(ctx context.Context, client *http.Client, binding Binding, since time.Time, budget *gitHubRetryBudget) ([]int, error) {
	next, err := f.restEndpointURL(binding, repositoryEndpoint(binding)+"/issues/events?per_page=100")
	if err != nil {
		return nil, err
	}
	var numbers []int
	visited := map[string]bool{}
	for next != "" {
		if visited[next] {
			return nil, fmt.Errorf("GitHub parent events pagination did not advance")
		}
		visited[next] = true
		current := next
		var page []parentIssueEvent
		headers, err := f.doJSON(ctx, client, gitHubJSONRequest{Method: http.MethodGet, URL: current, Resource: "GitHub parent events", Out: &page, RetryBudget: budget, Binding: binding})
		if err != nil {
			return nil, err
		}
		crossed := false
		// GitHub's repository issue-event feed is observed newest first. Validate
		// the whole page before stopping; equality must continue onto the next page.
		for _, event := range page {
			if event.CreatedAt == nil {
				return nil, fmt.Errorf("GitHub parent event missing created_at")
			}
			if event.CreatedAt.Before(since) {
				crossed = true
				continue
			}
			if event.Event == "parent_issue_added" || event.Event == "parent_issue_removed" {
				if event.Issue == nil || event.Issue.Number <= 0 {
					return nil, fmt.Errorf("GitHub parent event missing child issue number")
				}
				numbers = append(numbers, event.Issue.Number)
			}
		}
		if crossed {
			break
		}
		next, err = nextGitHubLinkURL(current, headers.Get("Link"))
		if err != nil {
			return nil, err
		}
		next, err = canonicalGitHubPageURL(binding, next)
		if err != nil {
			return nil, err
		}
	}
	return numbers, nil
}

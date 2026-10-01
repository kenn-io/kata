package githubsync

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// StatusLocator comes only from a verified repository issue API row.
type StatusLocator struct {
	ExternalID        string
	LegacyExternalIDs []string
	Number            int
}

// StatusLocatorSession supports bounded backfill without bulk content imports.
type StatusLocatorSession interface {
	StatusLocators(context.Context, Config, int) ([]StatusLocator, int, error)
}

// StatusLocators reads at most one 100-row page, regardless of content cutoffs.
// A returned next page of zero means this enumeration lap is exhausted.
func (s *httpFetcherBindingSession) StatusLocators(ctx context.Context, cfg Config, page int) ([]StatusLocator, int, error) {
	if err := cfg.Validate(); err != nil {
		return nil, 0, err
	}
	binding, err := normalizeBinding(cfg.Binding())
	if err != nil || binding != s.binding || cfg.RepoID <= 0 || page <= 0 {
		return nil, 0, fmt.Errorf("GitHub locator scan requires the bound repository and a positive page")
	}
	if err := s.verifyStatusRepository(ctx, cfg); err != nil {
		return nil, 0, err
	}
	query := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
	endpoint := repositoryEndpoint(binding) + "/issues?" + query.Encode()
	var rows []statusIssue
	var headers http.Header
	// The status request returns response headers through this wrapper, while
	// keeping decoding, cooldown, redirects and error sanitization shared.
	if err := s.statusRequestWithHeaders(ctx, http.MethodGet, endpoint, nil, &rows, nil, &headers); err != nil {
		return nil, 0, err
	}
	if rows == nil || len(rows) > 100 {
		return nil, 0, fmt.Errorf("invalid bounded GitHub locator response")
	}
	locators := make([]StatusLocator, 0, len(rows))
	ids, numbers := map[int64]bool{}, map[int]bool{}
	for _, row := range rows {
		if row.PullRequest != nil {
			continue
		}
		if err := s.validateStatusIssue(row, row.ID, row.Number); err != nil {
			return nil, 0, err
		}
		if ids[row.ID] || numbers[row.Number] {
			return nil, 0, fmt.Errorf("duplicate GitHub locator identity")
		}
		ids[row.ID], numbers[row.Number] = true, true
		locators = append(locators, StatusLocator{ExternalID: issueExternalID(row.Issue), LegacyExternalIDs: statusLocatorAliases(row.Issue), Number: row.Number})
	}
	next, err := s.nextStatusLocatorPage(cfg, page, headers.Get("Link"))
	if err != nil {
		return nil, 0, err
	}
	return locators, next, nil
}

func (s *httpFetcherBindingSession) nextStatusLocatorPage(cfg Config, page int, header string) (int, error) {
	link := nextGitHubLink(header)
	if link == "" {
		return 0, nil
	}
	base, err := s.fetcher.restEndpointURL(s.binding, repositoryEndpoint(s.binding)+"/issues")
	if err != nil {
		return 0, fmt.Errorf("invalid GitHub locator origin")
	}
	resolved, err := nextGitHubLinkURL(base, header)
	if err != nil {
		return 0, fmt.Errorf("invalid GitHub locator pagination")
	}
	current, err := url.Parse(base)
	if err != nil || current == nil {
		return 0, fmt.Errorf("invalid GitHub locator origin")
	}
	next, err := url.Parse(resolved)
	if err != nil || next == nil || next.User != nil || next.Fragment != "" || next.Scheme != current.Scheme || !strings.EqualFold(next.Host, current.Host) {
		return 0, fmt.Errorf("GitHub locator pagination leaves the bound origin")
	}
	prefix := strings.TrimSuffix(current.Path, "/repos/"+s.binding.Owner+"/"+s.binding.Repo+"/issues")
	numericPath := prefix + "/repositories/" + strconv.FormatInt(cfg.RepoID, 10) + "/issues"
	if !strings.EqualFold(next.EscapedPath(), current.EscapedPath()) && next.EscapedPath() != numericPath {
		return 0, fmt.Errorf("GitHub locator pagination leaves the bound repository")
	}
	query, err := url.ParseQuery(next.RawQuery)
	if err != nil {
		return 0, fmt.Errorf("invalid GitHub locator pagination query")
	}
	nextPage, err := strconv.Atoi(query.Get("page"))
	if err != nil || nextPage <= page || nextPage-page != 1 {
		return 0, fmt.Errorf("GitHub locator pagination must advance one page")
	}
	allowed := map[string]string{"state": "all", "sort": "created", "direction": "asc", "per_page": "100", "page": strconv.Itoa(nextPage)}
	for key, values := range query {
		want, ok := allowed[key]
		if !ok || len(values) != 1 || values[0] != want {
			return 0, fmt.Errorf("GitHub locator pagination changes the bounded scan")
		}
	}
	return nextPage, nil
}

func statusLocatorAliases(issue Issue) []string {
	aliases := issueLegacyExternalIDs(issue)
	return append(aliases, "issue-number:"+strconv.Itoa(issue.Number))
}

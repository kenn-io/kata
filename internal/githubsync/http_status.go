package githubsync

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

const maxStatusResponseBytes = 8 << 20

// StatusSession adds status-only operations to a repository-scoped session.
// The locator number must come from a verified provider response, not content.
type StatusSession interface {
	ReadStatus(context.Context, Config, string, int) (issuesync.StatusObservation, error)
	WriteStatus(context.Context, Config, string, int, string, func() error) (issuesync.StatusObservation, error)
}

var _ StatusSession = (*httpFetcherBindingSession)(nil)

type statusIssue struct {
	Issue
	URL string `json:"url"`
}

func (s *httpFetcherBindingSession) statusConfig(cfg Config, externalID string, number int) (int64, error) {
	if err := cfg.Validate(); err != nil {
		return 0, err
	}
	binding, err := normalizeBinding(cfg.Binding())
	if err != nil || binding != s.binding || cfg.RepoID <= 0 || number <= 0 {
		return 0, fmt.Errorf("GitHub status read requires the bound repository and trusted issue number")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(externalID, "issue-id:"), 10, 64)
	if err != nil || id <= 0 || externalID != "issue-id:"+strconv.FormatInt(id, 10) {
		return 0, fmt.Errorf("GitHub status requires a canonical global issue identity")
	}
	return id, nil
}

// ReadStatus verifies repository and issue identities independently of content.
func (s *httpFetcherBindingSession) ReadStatus(ctx context.Context, cfg Config, externalID string, number int) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	id, err := s.statusConfig(cfg, externalID, number)
	if err != nil {
		return zero, err
	}
	if err := s.verifyStatusRepository(ctx, cfg); err != nil {
		return zero, err
	}
	endpoint := repositoryEndpoint(s.binding) + "/issues/" + strconv.Itoa(number)
	var wire statusIssue
	if err := s.statusRequest(ctx, http.MethodGet, endpoint, nil, &wire, nil); err != nil {
		return zero, err
	}
	if err := s.validateStatusIssue(wire, id, number); err != nil {
		return zero, err
	}
	raw := wire.State
	obs := issuesync.StatusObservation{RawStatus: &raw, Status: raw, Version: wire.UpdatedAt.UTC(), Locator: strconv.Itoa(number)}
	if raw == "closed" {
		obs.ClosedReason = issueClosedReason(wire.Issue)
		obs.ClosedAt = wire.ClosedAt
	}
	return obs, nil
}

// verifyStatusRepository checks the bound repository identity once per session,
// which spans one sync run.
func (s *httpFetcherBindingSession) verifyStatusRepository(ctx context.Context, cfg Config) error {
	if s.verifiedRepoID == cfg.RepoID {
		return nil
	}
	var repo Repository
	if err := s.statusRequest(ctx, http.MethodGet, repositoryEndpoint(s.binding), nil, &repo, nil); err != nil {
		return err
	}
	if repo.ID != cfg.RepoID || !strings.EqualFold(repo.FullName, cfg.DisplayName()) {
		return fmt.Errorf("GitHub status repository identity changed")
	}
	s.verifiedRepoID = cfg.RepoID
	return nil
}

func (s *httpFetcherBindingSession) validateStatusIssue(wire statusIssue, id int64, number int) error {
	if id <= 0 || number <= 0 || wire.ID != id || wire.Number != number || wire.PullRequest != nil {
		return fmt.Errorf("GitHub status issue identity changed or is a pull request")
	}
	endpoint := repositoryEndpoint(s.binding) + "/issues/" + strconv.Itoa(number)
	expected, err := url.Parse(githubRESTBaseURL(s.binding) + "/" + endpoint)
	if err != nil || expected == nil {
		return fmt.Errorf("invalid GitHub status origin")
	}
	actual, err := url.Parse(wire.URL)
	if err != nil || actual == nil || actual.User != nil || actual.RawQuery != "" || actual.Fragment != "" || !strings.EqualFold(actual.Scheme, expected.Scheme) || !strings.EqualFold(actual.Host, expected.Host) || !strings.EqualFold(actual.EscapedPath(), expected.EscapedPath()) {
		return fmt.Errorf("GitHub status issue is outside the bound repository")
	}
	if wire.State != "open" && wire.State != "closed" || wire.UpdatedAt == nil || wire.UpdatedAt.IsZero() {
		return fmt.Errorf("GitHub status observation has invalid state or version")
	}
	return nil
}

// WriteStatus performs at most one PATCH and verifies its result by fresh read.
func (s *httpFetcherBindingSession) WriteStatus(ctx context.Context, cfg Config, externalID string, number int, desired string, admission func() error) (issuesync.StatusObservation, error) {
	var zero issuesync.StatusObservation
	if cfg.StatusSync != "two-way" || admission == nil || desired != "open" && desired != "closed" {
		return zero, fmt.Errorf("GitHub status writes require two-way mode, valid state and delivery admission")
	}
	id, err := s.statusConfig(cfg, externalID, number)
	if err != nil {
		return zero, err
	}
	observed, err := s.ReadStatus(ctx, cfg, externalID, number)
	if err != nil || observed.Status == desired {
		return observed, err
	}
	var wire statusIssue
	endpoint := repositoryEndpoint(s.binding) + "/issues/" + strconv.Itoa(number)
	if err := s.statusRequest(ctx, http.MethodPatch, endpoint, map[string]string{"state": desired}, &wire, admission); err != nil {
		return zero, err
	}
	if wire.ID != id || wire.Number != number || wire.PullRequest != nil {
		return zero, &issuesync.StatusError{Message: "GitHub status response has wrong object identity", Ambiguous: true}
	}
	observed, err = s.ReadStatus(ctx, cfg, externalID, number)
	if err != nil || observed.Status != desired {
		return zero, &issuesync.StatusError{Message: "GitHub status write could not be verified", Ambiguous: true}
	}
	return observed, nil
}

// statusRequest bounds and sanitizes responses. Failed GETs retry in the next
// status pass; PATCH failures must never enter the bulk fetcher's retry loop.
func (s *httpFetcherBindingSession) statusRequest(ctx context.Context, method, endpoint string, body any, out any, admission func() error) error {
	return s.statusRequestWithHeaders(ctx, method, endpoint, body, out, admission, nil)
}

func (s *httpFetcherBindingSession) statusRequestWithHeaders(ctx context.Context, method, endpoint string, body any, out any, admission func() error, headers *http.Header) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.fetcher.statusCooldownError(s.binding); err != nil {
		return err
	}
	requestURL, err := s.fetcher.restEndpointURL(s.binding, endpoint)
	if err != nil {
		return fmt.Errorf("invalid GitHub status endpoint")
	}
	var encoded []byte
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cannot encode GitHub status payload")
		}
	}
	attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	attemptCtx, sent := issuesync.TrackRequestWrite(attemptCtx)
	req, err := http.NewRequestWithContext(attemptCtx, method, requestURL, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("invalid GitHub status request")
	}
	req.Header.Set("Accept", githubRESTAcceptHeader)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersionHeader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := attemptCtx.Err(); err != nil {
		return err
	}
	if method == http.MethodPatch {
		if admission == nil {
			return fmt.Errorf("GitHub status write requires delivery admission")
		}
		if err := admission(); err != nil {
			return err
		}
		if err := attemptCtx.Err(); err != nil {
			return err
		}
	}
	client := *s.client
	client.CheckRedirect = noFollowGitHubRedirects
	resp, err := client.Do(req)
	if err != nil {
		return &issuesync.StatusError{Message: "GitHub status transport failed", Ambiguous: method == http.MethodPatch && sent()}
	}
	if headers != nil {
		*headers = resp.Header.Clone()
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStatusResponseBytes+1))
	_ = resp.Body.Close()
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	if readErr != nil || len(raw) > maxStatusResponseBytes || success && json.Unmarshal(raw, out) != nil {
		return &issuesync.StatusError{Message: "invalid GitHub status response", HTTPStatus: resp.StatusCode, Ambiguous: method == http.MethodPatch}
	}
	if success {
		return nil
	}
	retry := gitHubRESTStatusRetryable(resp.StatusCode, resp.Header, raw)
	wait := max(retryWait(resp.Header, s.fetcher.now()), 0)
	if retry {
		if resp.StatusCode == 429 && wait == 0 {
			wait = time.Second
		}
		s.fetcher.deferStatusRequests(s.binding, wait)
	}
	return &issuesync.StatusError{
		Message: fmt.Sprintf("GitHub status HTTP %d", resp.StatusCode), HTTPStatus: resp.StatusCode,
		Ambiguous:  method == http.MethodPatch && resp.StatusCode >= 500,
		Blocked:    !retry && resp.StatusCode != 409,
		RetryAfter: wait,
	}
}

func (f *HTTPFetcher) deferStatusRequests(binding Binding, delay time.Duration) {
	if delay <= 0 {
		return
	}
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	until := f.now().Add(delay)
	if until.After(f.statusCooldown[binding]) {
		if f.statusCooldown == nil {
			f.statusCooldown = map[Binding]time.Time{}
		}
		f.statusCooldown[binding] = until
	}
}

func (f *HTTPFetcher) statusCooldownError(binding Binding) error {
	f.statusMu.Lock()
	defer f.statusMu.Unlock()
	if wait := f.statusCooldown[binding].Sub(f.now()); wait > 0 {
		return &issuesync.StatusError{Message: "GitHub status requests are cooling down", RetryAfter: wait}
	}
	return nil
}

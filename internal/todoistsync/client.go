package todoistsync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/todoistsync/todoistapi"
)

// Fetcher resolves credential identity and opens an account-pinned read session.
type Fetcher interface {
	Account(context.Context) (string, error)
	ForRun(context.Context, Config) (Session, error)
}

// Session reads one selected project and its tasks.
type Session interface {
	Project(context.Context, Config) (Project, error)
	// Tasks returns every active task and the tasks completed in
	// [since, until), with since clamped to the history floor.
	Tasks(ctx context.Context, c Config, since, until time.Time) ([]Task, error)
}

// ClientConfig selects daemon-owned credentials and an optional transport.
type ClientConfig struct {
	Daemon    config.TodoistSyncConfig
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	// BackOff overrides the GET retry policy; tests use it to avoid waiting.
	BackOff func() backoff.BackOff
}

// Client builds sessions against Todoist's API at the configured origin.
type Client struct {
	cfg       ClientConfig
	configErr error
	http      *http.Client
}

type clientSession struct {
	client *Client
	config Config
	api    *todoistapi.Client
}

// NewClient constructs a client without reading credentials or contacting Todoist.
func NewClient(cfg ClientConfig) *Client {
	daemon, err := config.NormalizeTodoistSyncConfig(cfg.Daemon)
	cfg.Daemon = daemon
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.BackOff == nil {
		cfg.BackOff = func() backoff.BackOff { return backoff.NewExponentialBackOff() }
	}
	// Redirects are refused so the bearer token never leaves the configured origin.
	httpClient := &http.Client{Transport: cfg.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{cfg: cfg, configErr: err, http: httpClient}
}

func (c *Client) session(ctx context.Context) (*clientSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	token, ok := c.cfg.LookupEnv(c.cfg.Daemon.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		return nil, blocked("Todoist daemon token environment variable is unset")
	}
	api, err := todoistapi.NewDefaultClient(c.cfg.Daemon.APIOrigin,
		runtime.WithHTTPClient(retryingDoer{http: c.http, backOff: c.cfg.BackOff}),
		runtime.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
	if err != nil {
		return nil, err
	}
	return &clientSession{client: c, config: Config{APIOrigin: c.cfg.Daemon.APIOrigin}, api: api}, nil
}

// Account resolves only the credential's identity; no account data is imported.
func (c *Client) Account(ctx context.Context) (string, error) {
	s, err := c.session(ctx)
	if err != nil {
		return "", err
	}
	return s.account(ctx)
}

func (s *clientSession) account(ctx context.Context) (string, error) {
	resp, err := s.api.UserInfoAPIV1UserGetWithResponse(ctx)
	if err := responseError(err, "read Todoist account"); err != nil {
		return "", err
	}
	if s.config.AccountID != "" && resp.JSON200.ID != s.config.AccountID {
		return "", blocked("Todoist credential account differs from the saved binding")
	}
	return resp.JSON200.ID, nil
}

// ForRun opens a session for one binding after checking the credential's account.
func (c *Client) ForRun(ctx context.Context, input Config) (Session, error) {
	input, err := normalizeConfig(input)
	if err != nil {
		return nil, err
	}
	if input.APIOrigin != c.cfg.Daemon.APIOrigin {
		return nil, blocked("Todoist binding origin differs from daemon origin")
	}
	s, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	s.config = input
	if _, err = s.account(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Project reads the selected project and refuses archived or deleted projects.
func (s *clientSession) Project(ctx context.Context, c Config) (Project, error) {
	resp, err := s.api.GetProjectAPIV1ProjectsProjectIDGetWithResponse(ctx, &todoistapi.GetProjectAPIV1ProjectsProjectIDGetRequestOptions{
		PathParams: &todoistapi.GetProjectAPIV1ProjectsProjectIDGetPath{ProjectID: url.PathEscape(c.ProjectID)},
	})
	if err := responseError(err, "read Todoist project"); err != nil {
		return Project{}, err
	}
	var p Project
	if v := resp.JSON200.AnyProjectSyncViewResponse_AnyOf; v != nil && v.IsA() {
		p = Project{ID: v.A.ID, Name: v.A.Name, Archived: v.A.IsArchived, Deleted: v.A.IsDeleted}
	} else if v != nil && v.IsB() {
		p = Project{ID: v.B.ID, Name: v.B.Name, Archived: v.B.IsArchived, Deleted: v.B.IsDeleted}
	}
	if p.ID != c.ProjectID || p.Archived || p.Deleted {
		return Project{}, blocked("Todoist project is unavailable")
	}
	return p, nil
}

func blocked(message string) error { return &issuesync.StatusError{Message: message, Blocked: true} }

// responseError classifies a generated client error. Context errors pass
// through; 4xx responses other than 409 and 429 block the binding.
func responseError(err error, action string) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if statusErr, ok := errors.AsType[*issuesync.StatusError](err); ok {
		return statusErr
	}
	apiErr, ok := errors.AsType[*runtime.ClientAPIError](err)
	if !ok || apiErr.StatusCode() == 0 {
		return &issuesync.StatusError{Message: "cannot " + action}
	}
	code := apiErr.StatusCode()
	return &issuesync.StatusError{
		Message:    fmt.Sprintf("cannot %s (HTTP %d)", action, code),
		HTTPStatus: code,
		Blocked:    code >= 300 && code < 500 && code != http.StatusConflict && code != http.StatusTooManyRequests,
	}
}

// retryingDoer retries idempotent reads on 429 and 5xx with exponential
// backoff, honoring Retry-After. Writes go out once; the status runner owns
// their recovery.
type retryingDoer struct {
	http    *http.Client
	backOff func() backoff.BackOff
}

// Do sends Todoist API requests to the configured provider origin, not the Kata API.
// huma-check:external
func (d retryingDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	// The generated client builds every URL from the origin that
	// NormalizeTodoistSyncConfig allowlists.
	if req.Method != http.MethodGet {
		return d.http.Do(req) //nolint:gosec // G704: allowlisted Todoist origin.
	}
	return backoff.Retry(ctx, func() (*http.Response, error) {
		resp, err := d.http.Do(req.Clone(ctx)) //nolint:gosec // G704: allowlisted Todoist origin.
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil
		}
		_ = resp.Body.Close()
		delay := retryAfter(resp.Header.Get("Retry-After"))
		statusErr := &issuesync.StatusError{Message: fmt.Sprintf("Todoist API temporarily unavailable (HTTP %d)", resp.StatusCode), HTTPStatus: resp.StatusCode, RetryAfter: delay}
		if delay > 0 {
			return nil, backoff.RetryAfter(delay, statusErr)
		}
		return nil, statusErr
	}, backoff.WithBackOff(d.backOff()), backoff.WithMaxTries(4))
}

func retryAfter(header string) time.Duration {
	if seconds, err := strconv.Atoi(header); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(header); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
}

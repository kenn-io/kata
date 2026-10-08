package todoistsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
)

const maxResponseBytes = 8 << 20
const maxResponsePages = 1000

// Fetcher resolves credential identity and opens an account-pinned read session.
type Fetcher interface {
	Account(context.Context) (string, error)
	ForRun(context.Context, Config) (Session, error)
}

// Session reads one selected project, its active tasks and the completions
// recorded from since (clamped to the history floor) until the cutoff.
type Session interface {
	Project(context.Context, Config) (Project, error)
	Tasks(ctx context.Context, c Config, since, until time.Time) ([]Task, error)
}

// ClientConfig selects daemon-owned credentials and isolated transport/timing seams.
type ClientConfig struct {
	Daemon    config.TodoistSyncConfig
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client owns origin-pinned HTTP requests, pacing and shared retry cooldowns.
type Client struct {
	cfg            ClientConfig
	configErr      error
	http           *http.Client
	mu             sync.Mutex
	admission      chan struct{}
	next, cooldown time.Time
}
type clientSession struct {
	client *Client
	config Config
	token  string
	// scoped and history serve status reads within one run.
	scoped  bool
	history *completionHistory
}

// NewClient constructs a client without reading credentials or contacting Todoist.
func NewClient(cfg ClientConfig) *Client {
	daemon, err := config.NormalizeTodoistSyncConfig(cfg.Daemon)
	cfg.Daemon = daemon
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForTodoist
	}
	return &Client{cfg: cfg, configErr: err, admission: make(chan struct{}, 1), http: &http.Client{Transport: cfg.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
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
	if !ok || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, blocked("Todoist daemon token environment variable is unset or invalid")
	}
	return &clientSession{client: c, config: Config{APIOrigin: c.cfg.Daemon.APIOrigin}, token: token}, nil
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
	raw, err := s.get(ctx, "/api/v1/user")
	if err != nil {
		return "", err
	}
	var account struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &account) != nil || ValidateID(account.ID) != nil {
		return "", blocked("invalid Todoist account response")
	}
	if s.config.AccountID != "" && account.ID != s.config.AccountID {
		return "", blocked("Todoist credential account differs from the saved binding")
	}
	return account.ID, nil
}

// ForRun captures one credential and verifies its configured account identity.
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
func (s *clientSession) validate(input Config) error {
	c, err := normalizeConfig(input)
	if err != nil {
		return err
	}
	if c.SourceKey() != s.config.SourceKey() {
		return blocked("Todoist session scope differs from binding")
	}
	return nil
}
func blocked(message string) error { return &issuesync.StatusError{Message: message, Blocked: true} }
func waitForTodoist(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) admit(ctx context.Context) error {
	select {
	case c.admission <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.admission }()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	when := c.next
	if c.cooldown.After(when) {
		when = c.cooldown
	}
	c.mu.Unlock()
	if now := c.cfg.Now(); now.After(when) {
		when = now
	}
	for {
		if delay := when.Sub(c.cfg.Now()); delay > 0 {
			if err := c.cfg.Wait(ctx, delay); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		// An in-flight request may extend the shared cooldown while we wait.
		if c.cooldown.After(when) {
			when = c.cooldown
			c.mu.Unlock()
			continue
		}
		if now := c.cfg.Now(); now.After(when) {
			when = now
		}
		c.next = when.Add(time.Second)
		c.mu.Unlock()
		return nil
	}
}

func (c *Client) deferRequests(header string, attempt int) {
	now := c.cfg.Now()
	delay := time.Duration(1<<attempt) * time.Second
	if seconds, err := strconv.ParseInt(header, 10, 32); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil && at.After(now) {
		delay = at.Sub(now)
	}
	if delay > 20*time.Minute {
		delay = 20 * time.Minute
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	until := now.Add(delay)
	if until.After(c.cooldown) {
		c.cooldown = until
	}
}

// cooldownRemaining reports how long requests stay deferred after a rate limit.
func (c *Client) cooldownRemaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(c.cooldown.Sub(c.cfg.Now()), 0)
}

// get reads Todoist's API at the configured provider origin, not the Kata API.
// huma-check:external
func (s *clientSession) get(ctx context.Context, path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := s.client.admit(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.APIOrigin+path, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot build Todoist read request")
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Accept", "application/json")
		response, err := s.client.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, &issuesync.StatusError{Message: "Todoist read request failed"}
		}
		if response.StatusCode == 429 || response.StatusCode >= 500 {
			s.client.deferRequests(response.Header.Get("Retry-After"), attempt)
			_ = response.Body.Close()
			if attempt < 3 {
				continue
			}
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("Todoist API temporarily unavailable (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, RetryAfter: s.client.cooldownRemaining()}
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("Todoist API read failed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Blocked: response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != 409}
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
		_ = response.Body.Close()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, &issuesync.StatusError{Message: "cannot read Todoist API response"}
		}
		if len(raw) > maxResponseBytes {
			return nil, fmt.Errorf("todoist API response exceeds 8 MiB")
		}
		return raw, nil
	}
}

func (s *clientSession) Project(ctx context.Context, c Config) (Project, error) {
	if err := s.validate(c); err != nil {
		return Project{}, err
	}
	raw, err := s.get(ctx, projectAPIPath(c.ProjectID))
	if err != nil {
		return Project{}, err
	}
	var project Project
	if json.Unmarshal(raw, &project) != nil || project.ID != c.ProjectID || project.Archived == nil || project.Deleted == nil || *project.Archived || *project.Deleted {
		return Project{}, blocked("Todoist project is unavailable or identity does not match")
	}
	return project, nil
}

func projectAPIPath(id string) string {
	return strings.Join([]string{"/api/v1", "projects", id}, "/")
}

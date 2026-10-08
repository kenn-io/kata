package tickticksync

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

const apiOrigin = "https://api.ticktick.com"
const maxResponseBytes = 8 << 20
const maxCooldown = 15 * time.Minute

// errTaskOutsideProject marks a task read that returned another project's task,
// such as a task moved after it was imported.
var errTaskOutsideProject = errors.New("TickTick task is outside the selected project")

// Fetcher captures one daemon-owned credential per run.
type Fetcher interface {
	ForRun(context.Context, Config) (Session, error)
}

// Session offers only scoped public reads.
type Session interface {
	Project(context.Context) (Project, error)
	Data(context.Context) (ProjectData, error)
	Task(context.Context, string) (Task, error)
}

// ClientConfig injects clocks and transport without permitting credential origins.
type ClientConfig struct {
	TokenEnv  string
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client shares conservative pacing and cooldown across all bindings and runs.
type Client struct {
	cfg            ClientConfig
	configErr      error
	http           *http.Client
	mu             sync.Mutex
	gate           chan struct{}
	next, cooldown time.Time
}
type clientSession struct {
	client *Client
	config Config
	token  string
}

// NewClient pins requests to the public API and shares pacing across sessions.
func NewClient(cfg ClientConfig) *Client {
	daemon, err := config.NormalizeTickTickSyncConfig(config.TickTickSyncConfig{TokenEnv: cfg.TokenEnv})
	cfg.TokenEnv = daemon.TokenEnv
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForTickTick
	}
	return &Client{cfg: cfg, configErr: err, gate: make(chan struct{}, 1), http: &http.Client{Transport: cfg.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ForRun validates scope and captures the selected daemon credential once.
func (c *Client) ForRun(ctx context.Context, input Config) (Session, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	input, err := normalizeConfig(input)
	if err != nil {
		return nil, err
	}
	token, ok := c.cfg.LookupEnv(c.cfg.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("TickTick daemon token environment variable is unset or invalid")
	}
	return &clientSession{client: c, config: input, token: token}, nil
}
func waitForTickTick(ctx context.Context, d time.Duration) error {
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
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.gate }()
	for {
		c.mu.Lock()
		when := c.next
		if c.cooldown.After(when) {
			when = c.cooldown
		}
		c.mu.Unlock()
		if d := when.Sub(c.cfg.Now()); d > 0 {
			if err := c.cfg.Wait(ctx, d); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.mu.Lock()
		if c.cooldown.After(c.cfg.Now()) {
			c.mu.Unlock()
			continue
		}
		c.next = c.cfg.Now().Add(time.Second)
		c.mu.Unlock()
		return nil
	}
}
func (c *Client) deferRequests(header string) time.Duration {
	delay := 10 * time.Second
	if n, err := strconv.ParseInt(strings.TrimSpace(header), 10, 32); err == nil && n >= 0 {
		delay = time.Duration(n) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		delay = max(time.Duration(0), at.Sub(c.cfg.Now()))
	}
	delay = min(delay, maxCooldown)
	c.mu.Lock()
	until := c.cfg.Now().Add(delay)
	if until.After(c.cooldown) {
		c.cooldown = until
	}
	remaining := c.cooldown.Sub(c.cfg.Now())
	c.mu.Unlock()
	return remaining
}
func (s *clientSession) path() string { return "/open/v1/project/" + s.config.ProjectID }
func blocked(message string) error    { return &issuesync.StatusError{Message: message, Blocked: true} }
func (s *clientSession) get(ctx context.Context, path string, target any) error {
	for attempt := range 3 {
		if err := s.client.admit(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiOrigin+path, nil)
		if err != nil {
			return blocked("invalid TickTick request")
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Accept", "application/json")
		res, err := s.client.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &issuesync.StatusError{Message: "TickTick read transport failed"}
		}
		if res.StatusCode == 429 || res.StatusCode >= 500 {
			delay := s.client.deferRequests(res.Header.Get("Retry-After"))
			_ = res.Body.Close()
			if attempt < 2 {
				continue
			}
			return &issuesync.StatusError{Message: fmt.Sprintf("TickTick read failed (HTTP %d)", res.StatusCode), HTTPStatus: res.StatusCode, RetryAfter: delay}
		}
		if res.StatusCode != http.StatusOK {
			_ = res.Body.Close()
			return &issuesync.StatusError{Message: fmt.Sprintf("TickTick read failed (HTTP %d)", res.StatusCode), HTTPStatus: res.StatusCode, Blocked: true}
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
		_ = res.Body.Close()
		if readErr != nil {
			return &issuesync.StatusError{Message: "TickTick read response was incomplete"}
		}
		if len(raw) > maxResponseBytes || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return blocked("invalid or oversized TickTick response")
		}
		return nil
	}
	return fmt.Errorf("TickTick read retry exhausted")
}
func (s *clientSession) Project(ctx context.Context) (Project, error) {
	var p Project
	if err := s.get(ctx, s.path(), &p); err != nil {
		return p, err
	}
	return p, ValidateProject(s.config, p)
}
func (s *clientSession) Data(ctx context.Context) (ProjectData, error) {
	var data ProjectData
	if err := s.get(ctx, s.path()+"/data", &data); err != nil {
		return data, err
	}
	if err := ValidateProject(s.config, data.Project); err != nil {
		return data, err
	}
	if len(data.Tasks) > maxTasks {
		return data, blocked("TickTick collection exceeds 10000 tasks")
	}
	for _, t := range data.Tasks {
		if err := validateTask(s.config, t); err != nil {
			return data, err
		}
	}
	return data, nil
}
func (s *clientSession) Task(ctx context.Context, id string) (Task, error) {
	t, err := s.readTask(ctx, id)
	if err != nil {
		return t, err
	}
	return t, validateTask(s.config, t)
}

// readTask reads one scoped task without bounding its content, which status
// reads never import.
func (s *clientSession) readTask(ctx context.Context, id string) (Task, error) {
	var t Task
	if ValidateID(id) != nil {
		return t, blocked("invalid TickTick task identity")
	}
	if err := s.get(ctx, s.path()+"/task/"+id, &t); err != nil {
		return t, err
	}
	if t.ID != id {
		return t, blocked("TickTick task read returned another identity")
	}
	if t.ProjectID != s.config.ProjectID {
		return t, errTaskOutsideProject
	}
	if err := validateTaskIdentity(s.config, t); err != nil {
		return t, blocked(err.Error())
	}
	return t, nil
}

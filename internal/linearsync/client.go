package linearsync

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
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

const endpoint = "https://api.linear.app/graphql"
const maxResponseBytes = 8 << 20
const maxPages = 1000

// Fetcher opens a source session for one sync run.
type Fetcher interface {
	ForRun(context.Context, Config) (Session, error)
}

// Session reads the selected scope, workflow states, and issue collection.
type Session interface {
	Scope(context.Context, Config) (Scope, error)
	States(context.Context, Config) ([]State, error)
	Issues(context.Context, Config) ([]Issue, error)
}

// ClientConfig injects transport and clocks; the credential origin is fixed.
type ClientConfig struct {
	Daemon    config.LinearSyncConfig
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client shares authentication, request pacing, and cooldowns across sessions.
type Client struct {
	cfg            ClientConfig
	configErr      error
	http           *http.Client
	admission      chan struct{}
	mu             sync.Mutex
	next, cooldown time.Time
}
type clientSession struct {
	client        *Client
	config        Config
	authorization string
}

// NewClient constructs a client pinned to Linear's public GraphQL endpoint.
func NewClient(cfg ClientConfig) *Client {
	var err error
	cfg.Daemon, err = config.NormalizeLinearSyncConfig(cfg.Daemon)
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForLinear
	}
	return &Client{cfg: cfg, configErr: err, admission: make(chan struct{}, 1), http: &http.Client{Transport: cfg.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ForRun validates the scope and captures the daemon credential for one run.
func (c *Client) ForRun(ctx context.Context, input Config) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	input, err := normalizeConfig(input)
	if err != nil {
		return nil, err
	}
	token, ok := c.cfg.LookupEnv(c.cfg.Daemon.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("linear daemon token environment variable is unset or invalid")
	}
	if c.cfg.Daemon.AuthType == "oauth" {
		token = "Bearer " + token
	}
	return &clientSession{client: c, config: input, authorization: token}, nil
}
func waitForLinear(ctx context.Context, d time.Duration) error {
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
	c.mu.Lock()
	when := c.next
	if c.cooldown.After(when) {
		when = c.cooldown
	}
	c.mu.Unlock()
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
		if c.cooldown.After(when) {
			when = c.cooldown
			c.mu.Unlock()
			continue
		}
		if now := c.cfg.Now(); now.After(when) {
			when = now
		}
		c.next = when.Add(1500 * time.Millisecond)
		c.mu.Unlock()
		return nil
	}
}
func (c *Client) headersCooldown(h http.Header, limited bool, attempt int) {
	now := c.cfg.Now()
	until := time.Time{}
	if limited {
		delay := time.Duration(1<<attempt) * time.Second
		if seconds, err := strconv.ParseInt(h.Get("Retry-After"), 10, 32); err == nil && seconds >= 0 {
			delay = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(h.Get("Retry-After")); err == nil && at.After(now) {
			delay = at.Sub(now)
		}
		until = now.Add(delay)
	}
	for _, prefix := range []string{"X-RateLimit-Requests", "X-RateLimit-Endpoint-Requests", "X-RateLimit-Complexity"} {
		if h.Get(prefix+"-Remaining") == "0" {
			if ms, err := strconv.ParseInt(h.Get(prefix+"-Reset"), 10, 64); err == nil {
				at := time.UnixMilli(ms)
				if at.After(until) {
					until = at
				}
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.cooldown) {
		c.cooldown = until
	}
}
func (c *Client) cooldownRemaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(c.cooldown.Sub(c.cfg.Now()), 0)
}
func (s *clientSession) validate(c Config) error {
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	if c.SourceKey() != s.config.SourceKey() {
		return fmt.Errorf("linear session source identity does not match")
	}
	return nil
}

type graphError struct {
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}
type graphEnvelope struct {
	Data   jsontext.Value `json:"data"`
	Errors []graphError   `json:"errors"`
}

func graphClassification(errors []graphError) (limited, auth bool) {
	for _, e := range errors {
		switch strings.ToUpper(e.Extensions.Code) {
		case "RATELIMITED":
			limited = true
		case "AUTHENTICATION_ERROR", "AUTHENTICATION_REQUIRED", "FORBIDDEN", "UNAUTHENTICATED", "UNAUTHORIZED":
			auth = true
		}
	}
	return
}
func (s *clientSession) query(ctx context.Context, query string, variables map[string]any) (jsontext.Value, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("cannot encode Linear query")
	}
	for attempt := 0; ; attempt++ {
		if err := s.client.admit(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("cannot build Linear request")
		}
		req.Header.Set("Authorization", s.authorization)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := s.client.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, &issuesync.StatusError{Message: "linear read request failed"}
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
		_ = res.Body.Close()
		s.client.headersCooldown(res.Header, false, attempt)
		var envelope graphEnvelope
		decodeErr := json.Unmarshal(raw, &envelope)
		limited, auth := graphClassification(envelope.Errors)
		limited = limited || res.StatusCode == 429
		if limited || res.StatusCode >= 500 {
			s.client.headersCooldown(res.Header, true, attempt)
			if attempt < 3 {
				continue
			}
			return nil, &issuesync.StatusError{Message: "linear API temporarily unavailable", HTTPStatus: res.StatusCode, RetryAfter: s.client.cooldownRemaining()}
		}
		if res.StatusCode != 200 || auth {
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("linear API read failed (HTTP %d)", res.StatusCode), HTTPStatus: res.StatusCode, Blocked: true}
		}
		if readErr != nil || len(raw) > maxResponseBytes {
			return nil, &issuesync.StatusError{Message: "incomplete or oversized Linear response"}
		}
		if decodeErr != nil || len(envelope.Errors) > 0 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return nil, &issuesync.StatusError{Message: "invalid or partial Linear query response", Blocked: true}
		}
		return envelope.Data, nil
	}
}

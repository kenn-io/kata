package twentysync

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

const maxResponseBytes = 8 << 20
const maxResponsePages = 1000
const maxCollectionBytes = 128 << 20

// Fetcher captures one daemon-owned credential for each complete run.
type Fetcher interface {
	ForRun(context.Context, Config) (Session, error)
}

// Session reads authenticated workspace metadata and complete task collections.
// Only Workspace permits an empty workspace identity, for initial discovery.
type Session interface {
	Workspace(context.Context, Config) (Workspace, error)
	Schema(context.Context, Config) (Schema, error)
	// Tasks returns only tasks updated after the binding's cutoff.
	Tasks(context.Context, Config) ([]Task, error)
}

// ClientConfig injects transport and time without putting credentials in bindings.
type ClientConfig struct {
	Daemon    config.TwentySyncConfig
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client shares request pacing and server cooldowns across all project sessions.
type Client struct {
	cfg            ClientConfig
	configErr      error
	http           *http.Client
	mu             sync.Mutex
	admission      chan struct{}
	next, cooldown time.Time
}

// clientSession serves one sequential run. Its token cannot change workspaces,
// so it verifies identity and loads status options at most once.
type clientSession struct {
	client        *Client
	config        Config
	token         string
	verified      bool
	statusOptions []string
}

// NewClient constructs a shared, credential-pinned and paced API client.
func NewClient(c ClientConfig) *Client {
	daemon, err := config.NormalizeTwentySyncConfig(c.Daemon)
	c.Daemon = daemon
	if c.LookupEnv == nil {
		c.LookupEnv = os.LookupEnv
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Wait == nil {
		c.Wait = waitForTwenty
	}
	return &Client{cfg: c, configErr: err, admission: make(chan struct{}, 1), http: &http.Client{Transport: c.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ForRun pins the credential only after validating the daemon's exact API origin.
func (c *Client) ForRun(ctx context.Context, input Config) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.configErr != nil {
		return nil, c.configErr
	}
	input, err := normalizeSourceConfig(input, false)
	if err != nil {
		return nil, err
	}
	if input.APIOrigin != c.cfg.Daemon.APIOrigin {
		return nil, fmt.Errorf("twenty binding origin differs from daemon twenty_sync.api_origin")
	}
	token, ok := c.cfg.LookupEnv(c.cfg.Daemon.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("twenty daemon token environment variable is unset or invalid")
	}
	return &clientSession{client: c, config: input, token: token}, nil
}

func (s *clientSession) validate(c Config, bound bool) error {
	c, err := normalizeSourceConfig(c, bound)
	if err != nil {
		return err
	}
	if c.SourceKey() != s.config.SourceKey() {
		return fmt.Errorf("twenty session source identity does not match")
	}
	return nil
}

func waitForTwenty(ctx context.Context, d time.Duration) error {
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		when := maxTime(c.next, c.cooldown)
		c.mu.Unlock()
		if delay := when.Sub(c.cfg.Now()); delay > 0 {
			if err := c.cfg.Wait(ctx, delay); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
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

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (c *Client) deferRequests(header string, attempt int) {
	now := c.cfg.Now()
	delay := time.Duration(1<<attempt) * time.Second
	if seconds, err := strconv.ParseInt(header, 10, 32); err == nil && seconds >= 0 {
		delay = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil && at.After(now) {
		delay = at.Sub(now)
	}
	delay = min(delay, 20*time.Minute)
	c.mu.Lock()
	c.cooldown = maxTime(c.cooldown, now.Add(delay))
	c.mu.Unlock()
}
func (c *Client) cooldownRemaining() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return max(c.cooldown.Sub(c.cfg.Now()), 0)
}

func (s *clientSession) request(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.config.APIOrigin+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("cannot build Twenty API request")
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return s.client.http.Do(req)
}

// read retries only idempotent GETs and GraphQL queries, never status mutations.
func (s *clientSession) read(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := s.client.admit(ctx); err != nil {
			return nil, err
		}
		response, err := s.request(ctx, method, path, payload)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, &issuesync.StatusError{Message: "Twenty API read request failed"}
		}
		if response.StatusCode == 429 || response.StatusCode >= 500 {
			s.client.deferRequests(response.Header.Get("Retry-After"), attempt)
			_ = response.Body.Close()
			if attempt < 3 {
				continue
			}
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("Twenty API temporarily unavailable (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, RetryAfter: s.client.cooldownRemaining()}
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("Twenty API read failed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Blocked: response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != 409}
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
			return nil, fmt.Errorf("cannot read Twenty API response")
		}
		if len(raw) > maxResponseBytes {
			return nil, fmt.Errorf("twenty API response exceeds 8 MiB")
		}
		return raw, nil
	}
}

func (s *clientSession) graphql(ctx context.Context, query string, variables map[string]any) ([]byte, error) {
	return s.graphqlBounded(ctx, query, variables, nil)
}

func (s *clientSession) graphqlBounded(ctx context.Context, query string, variables map[string]any, used *int) ([]byte, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("cannot encode Twenty metadata query")
	}
	raw, err := s.read(ctx, http.MethodPost, "/metadata", payload)
	if err != nil {
		return nil, err
	}
	if used != nil {
		*used += len(raw)
		if *used > maxCollectionBytes {
			return nil, fmt.Errorf("twenty metadata exceeds 128 MiB")
		}
	}
	var envelope struct {
		Data   jsontext.Value `json:"data"`
		Errors jsontext.Value `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" || (len(envelope.Errors) > 0 && string(envelope.Errors) != "[]" && string(envelope.Errors) != "null") {
		return nil, fmt.Errorf("twenty metadata query failed or returned an unsupported schema")
	}
	return envelope.Data, nil
}

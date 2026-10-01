package notionsync

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/issuesync"
)

const notionOrigin = "https://api.notion.com"
const maxResponseBytes = 8 << 20
const requestSpacing = time.Second/3 + 1

// Fetcher captures one daemon-owned credential for a complete run.
type Fetcher interface {
	ForRun(context.Context) (Session, error)
}

// Session provides only the read operations needed by the Notion adapter.
type Session interface {
	Database(context.Context, string) (Database, error)
	DataSource(context.Context, string) (DataSource, error)
	Pages(context.Context, Config, *time.Time) ([]Page, error)
	Content(context.Context, Config, Page) (PageContent, error)
}

// ClientConfig permits clock and transport injection without changing API origin.
type ClientConfig struct {
	TokenEnv  string
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client shares admission pacing and upstream cooldowns across all run sessions.
type Client struct {
	cfg            ClientConfig
	http           *http.Client
	mu             sync.Mutex
	next, cooldown time.Time
	cooldownErr    error
}
type clientSession struct {
	client *Client
	token  string
}

var _ Fetcher = (*Client)(nil)
var _ Session = (*clientSession)(nil)

// NewClient constructs the daemon's fixed-origin, redirect-rejecting client.
func NewClient(cfg ClientConfig) *Client {
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = "KATA_NOTION_TOKEN"
	}
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForNotion
	}
	return &Client{cfg: cfg, http: &http.Client{Transport: cfg.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ForRun captures one daemon-owned credential for the complete run.
func (c *Client) ForRun(ctx context.Context) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	token, ok := c.cfg.LookupEnv(c.cfg.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" {
		return nil, fmt.Errorf("notion token environment variable is unset or blank")
	}
	return &clientSession{client: c, token: token}, nil
}
func waitForNotion(ctx context.Context, d time.Duration) error {
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		now := c.cfg.Now()
		ready := c.next
		if c.cooldown.After(ready) {
			ready = c.cooldown
		}
		delay := ready.Sub(now)
		if delay <= 0 {
			c.next = now.Add(requestSpacing)
			c.mu.Unlock()
			return nil
		}
		cooldownErr := c.cooldownErr
		cooling := c.cooldown.After(now)
		c.mu.Unlock()
		if deadline, ok := ctx.Deadline(); ok && !now.Add(delay).Before(deadline) {
			if cooling {
				return fmt.Errorf("%w: retry cooldown exceeds deadline (retry in %s)", cooldownErr, delay.Round(time.Second))
			}
			return context.DeadlineExceeded
		}
		if err := c.cfg.Wait(ctx, delay); err != nil {
			return err
		}
	}
}
func (c *Client) deferRequests(delay time.Duration, cause error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until := c.cfg.Now().Add(delay)
	if until.After(c.cooldown) {
		c.cooldown = until
		c.cooldownErr = cause
	}
}

// request bounds decoded bytes before parsing and never reports raw remote data.
// POST is used exclusively for the idempotent data-source query operation.
func (s *clientSession) request(ctx context.Context, method, path string, body any, out any) error {
	return s.requestWithAdmission(ctx, method, path, body, out, nil)
}

func (s *clientSession) requestWithAdmission(ctx context.Context, method, path string, body any, out any, admission func() error) error {
	mutating := method == http.MethodPatch
	if mutating && admission == nil {
		return fmt.Errorf("notion status writes require delivery admission")
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cannot encode Notion query")
		}
	}
	for attempt := range 5 {
		if err := s.client.admit(ctx); err != nil {
			return err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		attemptCtx, sent := issuesync.TrackRequestWrite(attemptCtx)
		req, err := http.NewRequestWithContext(attemptCtx, method, notionOrigin+path, bytes.NewReader(payload))
		if err != nil {
			cancel()
			return fmt.Errorf("invalid Notion request path")
		}
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("Notion-Version", "2026-03-11")
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if mutating {
			if err := admission(); err != nil {
				cancel()
				return err
			}
			if err := attemptCtx.Err(); err != nil {
				cancel()
				return err
			}
		}
		res, err := s.client.http.Do(req)
		var raw []byte
		status := 0
		retryAfter := ""
		if err == nil {
			status = res.StatusCode
			retryAfter = res.Header.Get("Retry-After")
			raw, err = io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
			_ = res.Body.Close()
		}
		attemptErr := attemptCtx.Err()
		cancel()
		if mutating {
			contextErr := ctx.Err()
			if contextErr == nil {
				contextErr = attemptErr
			}
			return s.statusResponse(raw, status, retryAfter, err, contextErr, sent(), out)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(raw) > maxResponseBytes {
			return fmt.Errorf("notion response exceeds 8 MiB")
		}
		if err == nil && status >= 200 && status < 300 {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("invalid Notion response JSON")
			}
			return nil
		}
		var remote struct {
			Code       string `json:"code"`
			Additional struct {
				Reason string `json:"rate_limit_reason"`
			} `json:"additional_data"`
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &remote)
		}
		if status == 429 && remote.Additional.Reason == "public_api_request_blocked" {
			return &issuesync.StatusError{Message: "notion HTTP 429: public_api_request_blocked", HTTPStatus: status, Blocked: true}
		}
		retryable := (err != nil && (status == 0 || (status >= 200 && status < 300))) || status == 429 || status == 529 || status == 500 || status == 502 || status == 503 || status == 504
		safeErr := fmt.Errorf("notion HTTP %d: %s", status, safeNotionCode(remote.Code))
		if err != nil {
			if status == 0 {
				safeErr = fmt.Errorf("notion transport or response read failed")
			} else {
				safeErr = fmt.Errorf("notion HTTP %d: %s (response read failed)", status, safeNotionCode(remote.Code))
			}
			if errors.Is(attemptErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
				safeErr = fmt.Errorf("%v: %w", safeErr, context.DeadlineExceeded)
			}
		}
		if !retryable {
			if err == nil {
				return &issuesync.StatusError{Message: safeErr.Error(), HTTPStatus: status, Blocked: status >= 300 && status < 500 && status != 409 && status != 429}
			}
			return safeErr
		}
		//nolint:gosec // Retry jitter is non-security randomness, never a credential or identifier.
		delay := time.Second*time.Duration(1<<attempt) + time.Duration(rand.Int64N(int64(250*time.Millisecond)+1))
		if wait := notionRetryAfter(retryAfter, s.client.cfg.Now()); wait > delay {
			delay = wait
		}
		s.client.deferRequests(delay, safeErr)
		if attempt == 4 {
			return safeErr
		}
	}
	panic("unreachable")
}

// statusResponse returns after one PATCH dispatch. A lost response or server
// failure may follow a committed change, so callers must re-read before retry.
func (s *clientSession) statusResponse(raw []byte, status int, retryAfter string, readErr, contextErr error, sent bool, out any) error {
	if readErr == nil && contextErr == nil && len(raw) <= maxResponseBytes && status >= 200 && status < 300 {
		if json.Unmarshal(raw, out) == nil {
			return nil
		}
		return &issuesync.StatusError{Message: "invalid Notion status response JSON", HTTPStatus: status, Ambiguous: true}
	}
	var remote struct {
		Code       string `json:"code"`
		Additional struct {
			Reason string `json:"rate_limit_reason"`
		} `json:"additional_data"`
	}
	if len(raw) <= maxResponseBytes {
		_ = json.Unmarshal(raw, &remote)
	}
	delivery := &issuesync.StatusError{
		Message:    fmt.Sprintf("notion HTTP %d: %s", status, safeNotionCode(remote.Code)),
		HTTPStatus: status,
		Ambiguous:  status == 0 && sent || status >= 500 || status >= 200 && status < 300,
		Blocked:    status >= 300 && status < 400 || status >= 400 && status < 500 && status != 409 && status != 429,
	}
	if readErr != nil || contextErr != nil || len(raw) > maxResponseBytes {
		delivery.Message = "notion status transport or response read failed"
	}
	if status == 429 && remote.Additional.Reason == "public_api_request_blocked" {
		delivery.Message = "notion HTTP 429: public_api_request_blocked"
		delivery.Blocked = true
		return delivery
	}
	if status == 429 || status >= 500 || status == 0 {
		delivery.RetryAfter = max(time.Second, notionRetryAfter(retryAfter, s.client.cfg.Now()))
		s.client.deferRequests(delivery.RetryAfter, delivery)
	}
	return delivery
}
func safeNotionCode(code string) string {
	switch code {
	case "invalid_json", "invalid_request_url", "invalid_request", "validation_error", "missing_version", "unauthorized", "restricted_resource", "object_not_found", "conflict_error", "rate_limited", "internal_server_error", "bad_gateway", "service_unavailable", "database_connection_unavailable", "gateway_timeout", "service_overload":
		return code
	default:
		return "request failed"
	}
}
func notionRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return time.Duration(min(seconds, uint64(runTimeout/time.Second))) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return min(at.Sub(now), runTimeout)
	}
	return 0
}
func validateResponseID(object, wantObject, id, wantID string) (string, error) {
	canonical, err := canonicalID(id)
	if err != nil || object != wantObject || canonical != wantID {
		return "", fmt.Errorf("notion response has wrong object identity")
	}
	return canonical, nil
}
func (s *clientSession) Database(ctx context.Context, id string) (Database, error) {
	id, err := canonicalID(id)
	if err != nil {
		return Database{}, err
	}
	var wire struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Sources []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data_sources"`
	}
	if err := s.request(ctx, http.MethodGet, "/v1/databases/"+id, nil, &wire); err != nil {
		return Database{}, err
	}
	if _, err := validateResponseID(wire.Object, "database", wire.ID, id); err != nil {
		return Database{}, err
	}
	result := Database{ID: id}
	seen := map[string]bool{}
	for _, source := range wire.Sources {
		sourceID, err := canonicalID(source.ID)
		if err != nil || seen[sourceID] {
			return Database{}, fmt.Errorf("invalid Notion database data source identity")
		}
		seen[sourceID] = true
		result.DataSources = append(result.DataSources, Option{ID: sourceID, Name: source.Name})
	}
	return result, nil
}
func (s *clientSession) DataSource(ctx context.Context, id string) (DataSource, error) {
	id, err := canonicalID(id)
	if err != nil {
		return DataSource{}, err
	}
	var wire struct {
		Object string     `json:"object"`
		ID     string     `json:"id"`
		Parent wireParent `json:"parent"`
		Title  []struct {
			PlainText string `json:"plain_text"`
		} `json:"title"`
		Properties map[string]struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Status struct {
				Options []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"options"`
				Groups []struct {
					ID        string   `json:"id"`
					Name      string   `json:"name"`
					OptionIDs []string `json:"option_ids"`
				} `json:"groups"`
			} `json:"status"`
		} `json:"properties"`
	}
	if err := s.request(ctx, http.MethodGet, "/v1/data_sources/"+id, nil, &wire); err != nil {
		return DataSource{}, err
	}
	if _, err := validateResponseID(wire.Object, "data_source", wire.ID, id); err != nil {
		return DataSource{}, err
	}
	parent, err := canonicalID(wire.Parent.DatabaseID)
	if err != nil || wire.Parent.Type != "database_id" {
		return DataSource{}, fmt.Errorf("invalid Notion data source parent")
	}
	result := DataSource{ID: id, DatabaseID: parent}
	for _, part := range wire.Title {
		result.Name += part.PlainText
	}
	seen := map[string]bool{}
	for name, property := range wire.Properties {
		if strings.TrimSpace(property.ID) == "" || property.Type == "" || seen[property.ID] {
			return DataSource{}, fmt.Errorf("invalid Notion data source property")
		}
		seen[property.ID] = true
		p := Property{ID: property.ID, Name: name, Type: property.Type}
		for _, option := range property.Status.Options {
			if option.ID == "" {
				return DataSource{}, fmt.Errorf("invalid Notion status option")
			}
			p.Options = append(p.Options, Option{ID: option.ID, Name: option.Name})
		}
		for _, group := range property.Status.Groups {
			p.Groups = append(p.Groups, Group{ID: group.ID, Name: group.Name, OptionIDs: group.OptionIDs})
		}
		result.Properties = append(result.Properties, p)
	}
	sort.Slice(result.Properties, func(i, j int) bool { return result.Properties[i].ID < result.Properties[j].ID })
	return result, nil
}

package planesync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

// Fetcher captures a daemon-owned credential for a complete source run.
type Fetcher interface {
	ForRun(context.Context, Config) (Session, error)
}

// Session exposes only the public reads needed by the importer.
type Session interface {
	Project(context.Context, Config) (Project, error)
	States(context.Context, Config) ([]State, error)
	WorkItems(context.Context, Config) ([]WorkItem, error)
}

// ClientConfig injects transport and clocks while keeping credentials pinned to
// daemon configuration, independently of restored or client-supplied bindings.
type ClientConfig struct {
	Daemon    config.PlaneSyncConfig
	LookupEnv func(string) (string, bool)
	Transport http.RoundTripper
	Now       func() time.Time
	Wait      func(context.Context, time.Duration) error
}

// Client shares request admission and server cooldowns across all sessions.
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
}

// NewClient creates a read-only client with one shared pacing gate.
func NewClient(cfg ClientConfig) *Client {
	daemon, err := config.NormalizePlaneSyncConfig(cfg.Daemon)
	cfg.Daemon = daemon
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wait == nil {
		cfg.Wait = waitForPlane
	}
	return &Client{admission: make(chan struct{}, 1), cfg: cfg, configErr: err, http: &http.Client{Transport: cfg.Transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// ForRun validates the saved origin before resolving or using credentials.
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
	if input.APIOrigin != c.cfg.Daemon.APIOrigin {
		return nil, fmt.Errorf("plane binding origin differs from daemon plane_sync.api_origin")
	}
	token, ok := c.cfg.LookupEnv(c.cfg.Daemon.TokenEnv)
	token = strings.TrimSpace(token)
	if !ok || token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("plane daemon token environment variable is unset or invalid")
	}
	return &clientSession{client: c, config: input, token: token}, nil
}

func waitForPlane(ctx context.Context, d time.Duration) error {
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

func (s *clientSession) validate(input Config) error {
	input, err := normalizeConfig(input)
	if err != nil {
		return err
	}
	if input.SourceKey() != s.config.SourceKey() {
		return fmt.Errorf("plane session source identity does not match")
	}
	return nil
}

func (s *clientSession) path() string {
	return "/api/v1/workspaces/" + s.config.Workspace + "/projects/" + s.config.ProjectID + "/"
}

func (s *clientSession) get(ctx context.Context, path string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := s.client.admit(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.config.APIOrigin+path, nil)
		if err != nil {
			return nil, fmt.Errorf("cannot build Plane read request")
		}
		req.Header.Set("X-API-Key", s.token)
		req.Header.Set("Accept", "application/json")
		response, err := s.client.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, &issuesync.StatusError{Message: "plane read request failed"}
		}
		if response.StatusCode == 429 || response.StatusCode >= 500 {
			s.client.deferRequests(response.Header.Get("Retry-After"), attempt)
			_ = response.Body.Close()
			if attempt < 3 {
				continue
			}
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("plane API temporarily unavailable (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, RetryAfter: s.client.cooldownRemaining()}
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, &issuesync.StatusError{Message: fmt.Sprintf("plane API read failed (HTTP %d)", response.StatusCode), HTTPStatus: response.StatusCode, Blocked: response.StatusCode >= 300 && response.StatusCode < 500 && response.StatusCode != 409}
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
			return nil, &issuesync.StatusError{Message: "cannot read Plane API response"}
		}
		if len(raw) > maxResponseBytes {
			return nil, fmt.Errorf("plane API response exceeds 8 MiB")
		}
		return raw, nil
	}
}

// Project validates the canonical project returned by the selected endpoint.
func (s *clientSession) Project(ctx context.Context, input Config) (Project, error) {
	if err := s.validate(input); err != nil {
		return Project{}, err
	}
	raw, err := s.get(ctx, s.path())
	if err != nil {
		return Project{}, err
	}
	var wire struct {
		ID         string  `json:"id"`
		Name       string  `json:"name"`
		Identifier string  `json:"identifier"`
		ArchivedAt *string `json:"archived_at"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Project{}, fmt.Errorf("invalid Plane project response")
	}
	id, err := CanonicalID(wire.ID)
	if err != nil || id != s.config.ProjectID || strings.TrimSpace(wire.Identifier) == "" || wire.ArchivedAt != nil {
		return Project{}, fmt.Errorf("plane project identity or availability does not match")
	}
	return Project{ID: id, Name: wire.Name, Identifier: wire.Identifier}, nil
}

func readRows[T any](ctx context.Context, s *clientSession, resource string, allowArray bool) ([]T, error) {
	var rows []T
	cursor := ""
	seen := map[string]bool{}
	totalBytes := 0
	for page := range maxResponsePages {
		path := s.path() + resource + "/?per_page=100"
		if resource == "work-items" {
			path += "&order_by=sequence_id"
		}
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		raw, err := s.get(ctx, path)
		if err != nil {
			return nil, err
		}
		totalBytes += len(raw)
		if totalBytes > 128<<20 {
			return nil, fmt.Errorf("plane collection responses exceed 128 MiB")
		}
		if allowArray && page == 0 && strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			if err := json.Unmarshal(raw, &rows); err != nil {
				return nil, fmt.Errorf("invalid Plane collection response")
			}
			if len(rows) > maxItems {
				return nil, fmt.Errorf("plane collection exceeds 10000 results")
			}
			return rows, nil
		}
		var envelope struct {
			Results *[]T   `json:"results"`
			Next    *bool  `json:"next_page_results"`
			Cursor  string `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Results == nil || envelope.Next == nil {
			return nil, fmt.Errorf("incomplete Plane pagination response")
		}
		if len(rows)+len(*envelope.Results) > maxItems {
			return nil, fmt.Errorf("plane collection exceeds 10000 results")
		}
		rows = append(rows, (*envelope.Results)...)
		if !*envelope.Next {
			return rows, nil
		}
		if envelope.Cursor == "" || seen[envelope.Cursor] {
			return nil, fmt.Errorf("missing or repeated Plane pagination cursor")
		}
		seen[envelope.Cursor] = true
		cursor = envelope.Cursor
	}
	return nil, fmt.Errorf("plane pagination exceeds 1000 responses")
}

type stateWire struct {
	ID       string  `json:"id"`
	Group    string  `json:"group"`
	Sequence float64 `json:"sequence"`
	Project  string  `json:"project"`
}

// States reads the complete state schema before work-item mapping.
func (s *clientSession) States(ctx context.Context, input Config) ([]State, error) {
	if err := s.validate(input); err != nil {
		return nil, err
	}
	rows, err := readRows[stateWire](ctx, s, "states", true)
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(rows))
	for _, row := range rows {
		id, err := CanonicalID(row.ID)
		if err != nil {
			return nil, err
		}
		if row.Project != "" {
			project, err := CanonicalID(row.Project)
			if err != nil || project != s.config.ProjectID {
				return nil, fmt.Errorf("plane state belongs to a different project")
			}
		}
		states = append(states, State{ID: id, Group: row.Group, Sequence: row.Sequence})
	}
	if _, err := stateGroups(states); err != nil {
		return nil, err
	}
	return states, nil
}

type itemWire struct {
	ID              string    `json:"id"`
	Project         string    `json:"project"`
	State           *string   `json:"state"`
	SequenceID      int64     `json:"sequence_id"`
	Name            *string   `json:"name"`
	Priority        *string   `json:"priority"`
	DescriptionHTML *string   `json:"description_html"`
	CreatedBy       *string   `json:"created_by"`
	Assignees       *[]string `json:"assignees"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ArchivedAt      *string   `json:"archived_at"`
	DeletedAt       *string   `json:"deleted_at"`
}

// WorkItems reads complete descriptions and unexpanded identities, never using
// undocumented incremental filters or dropping missing response fields.
func (s *clientSession) WorkItems(ctx context.Context, input Config) ([]WorkItem, error) {
	if err := s.validate(input); err != nil {
		return nil, err
	}
	rows, err := readRows[jsontext.Value](ctx, s, "work-items", false)
	if err != nil {
		return nil, err
	}
	items := make([]WorkItem, 0, len(rows))
	seen := map[string]bool{}
	for _, raw := range rows {
		var row itemWire
		if err := json.Unmarshal(raw, &row); err != nil {
			var identity struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &identity)
			return nil, fmt.Errorf("plane work item %q: invalid response fields", identity.ID)
		}
		id, err := CanonicalID(row.ID)
		if err != nil {
			return nil, fmt.Errorf("plane work item %q: %w", row.ID, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("plane work item %q: duplicate identity", row.ID)
		}
		seen[id] = true
		project, err := CanonicalID(row.Project)
		if err != nil || project != s.config.ProjectID {
			return nil, fmt.Errorf("plane work item %q: belongs to a different project", row.ID)
		}
		if row.ArchivedAt != nil || row.DeletedAt != nil {
			continue
		}
		if row.State == nil || row.Name == nil || row.DescriptionHTML == nil || row.Assignees == nil || row.Priority == nil {
			return nil, fmt.Errorf("plane work item %q: incomplete response", row.ID)
		}
		var priority *int64
		switch *row.Priority {
		case "urgent":
			priority = new(int64(0))
		case "high":
			priority = new(int64(1))
		case "medium":
			priority = new(int64(2))
		case "low":
			priority = new(int64(3))
		case "none":
		default:
			return nil, fmt.Errorf("plane work item %q: invalid priority", row.ID)
		}
		state, err := CanonicalID(*row.State)
		if err != nil {
			return nil, fmt.Errorf("plane work item %q: %w", row.ID, err)
		}
		if !validSourceTime(row.CreatedAt) || !validSourceTime(row.UpdatedAt) || row.UpdatedAt.Before(row.CreatedAt) || row.SequenceID < 1 {
			return nil, fmt.Errorf("plane work item %q: invalid sequence or timestamps", row.ID)
		}
		if len(*row.DescriptionHTML) > maxDescriptionBytes {
			return nil, fmt.Errorf("plane work item %q: description exceeds 1 MiB", row.ID)
		}
		creator := ""
		if row.CreatedBy != nil && *row.CreatedBy != "" {
			creator, err = CanonicalID(*row.CreatedBy)
			if err != nil {
				return nil, fmt.Errorf("plane work item %q: %w", row.ID, err)
			}
		}
		assignees := make([]string, 0, len(*row.Assignees))
		for _, value := range *row.Assignees {
			user, err := CanonicalID(value)
			if err != nil {
				return nil, fmt.Errorf("plane work item %q: %w", row.ID, err)
			}
			assignees = append(assignees, user)
		}
		items = append(items, WorkItem{ID: id, ProjectID: project, StateID: state, SequenceID: row.SequenceID, Name: *row.Name, Priority: priority, DescriptionHTML: *row.DescriptionHTML, CreatorID: creator, AssigneeIDs: assignees, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt})
	}
	return items, nil
}

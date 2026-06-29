package githubsync

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	actorGitHubSync = "github-sync"
	githubGhost     = "github-ghost"
)

// User is the subset of a GitHub user object needed for imports.
type User struct {
	Login string `json:"login"`
}

// Label is the subset of a GitHub label object needed for imports.
type Label struct {
	Name string `json:"name"`
}

// PullRequest marks an issue API row as a pull request.
type PullRequest struct{}

// Issue is the subset of a GitHub issue API row needed for import mapping.
type Issue struct {
	ID          int64        `json:"id"`
	NodeID      string       `json:"node_id"`
	Number      int          `json:"number"`
	HTMLURL     string       `json:"html_url"`
	Title       string       `json:"title"`
	Body        string       `json:"body"`
	State       string       `json:"state"`
	StateReason *string      `json:"state_reason"`
	Comments    int          `json:"comments"`
	User        *User        `json:"user"`
	Assignees   []User       `json:"assignees"`
	Labels      []Label      `json:"labels"`
	CreatedAt   *time.Time   `json:"created_at"`
	UpdatedAt   *time.Time   `json:"updated_at"`
	ClosedAt    *time.Time   `json:"closed_at"`
	PullRequest *PullRequest `json:"pull_request"`
}

// Comment is the subset of a GitHub issue comment API row needed for imports.
type Comment struct {
	ID        int64      `json:"id"`
	NodeID    string     `json:"node_id"`
	Body      string     `json:"body"`
	User      *User      `json:"user"`
	CreatedAt *time.Time `json:"created_at"`
}

// Repository is the subset of a GitHub repository object needed for sync setup.
type Repository struct {
	NodeID   string `json:"node_id"`
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// Binding identifies the GitHub repository associated with a kata project.
type Binding struct {
	Host  string
	Owner string
	Repo  string
}

// Config is the GitHub-owned issue sync binding configuration stored in the
// provider-neutral issue_sync_bindings.config_json column.
type Config struct {
	Host               string `json:"host"`
	Owner              string `json:"owner"`
	Repo               string `json:"repo"`
	RepoID             int64  `json:"repo_id"`
	TitlePrefix        *bool  `json:"title_prefix"`
	ParentLinksVersion int    `json:"parent_links_version,omitempty"`
}

const currentParentLinksVersion = 1

// Binding converts the stored config into the fetcher binding shape.
func (c Config) Binding() Binding {
	return Binding{Host: c.Host, Owner: c.Owner, Repo: c.Repo}
}

// DisplayName returns the human-readable GitHub repository name for this config.
func (c Config) DisplayName() string {
	return c.Owner + "/" + c.Repo
}

// UseTitlePrefix reports whether imported issue titles should include the
// upstream GitHub issue number. The default is true for legacy configs that do
// not carry the field.
func (c Config) UseTitlePrefix() bool {
	if c.TitlePrefix == nil {
		return true
	}
	return *c.TitlePrefix
}

// NeedsParentLinkBackfill reports whether an existing binding still needs a
// full issue pass to populate source-managed parent links.
func (c Config) NeedsParentLinkBackfill() bool {
	return c.ParentLinksVersion < currentParentLinksVersion
}

// WithParentLinksBackfilled marks the config as having completed the current
// parent-link backfill version.
func (c Config) WithParentLinksBackfilled() Config {
	c.ParentLinksVersion = currentParentLinksVersion
	return c
}

// Validate reports whether the config has the GitHub repository identity needed
// for fetch operations.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" || strings.TrimSpace(c.Owner) == "" || strings.TrimSpace(c.Repo) == "" {
		return fmt.Errorf("GitHub sync config requires host, owner, and repo")
	}
	return nil
}

// EncodeConfig validates and marshals a GitHub sync config for storage in an
// issue sync binding.
func EncodeConfig(c Config) (json.RawMessage, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c = c.withDefaults()
	bs, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode GitHub sync config: %w", err)
	}
	return bs, nil
}

// DecodeConfig unmarshals and validates a GitHub sync config from an issue sync
// binding.
func DecodeConfig(raw json.RawMessage) (Config, error) {
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("decode GitHub sync config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) withDefaults() Config {
	if c.TitlePrefix == nil {
		titlePrefix := true
		c.TitlePrefix = &titlePrefix
	}
	return c
}

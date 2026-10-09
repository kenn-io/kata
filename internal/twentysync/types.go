// Package twentysync maps Twenty workspace tasks to native Kata issues.
package twentysync

import "time"

// Config is the nonsecret binding identity, content policy and status mapping.
type Config struct {
	APIOrigin    string   `json:"api_origin"`
	WebOrigin    string   `json:"web_origin"`
	WorkspaceID  string   `json:"workspace_id"`
	Since        string   `json:"since"`
	TitlePrefix  *bool    `json:"title_prefix"`
	StatusSync   string   `json:"status_sync"`
	ClosedStatus string   `json:"closed_status"`
	OpenStatus   string   `json:"open_status"`
	OpenStatuses []string `json:"open_statuses"`
}

// SourceKey distinguishes workspaces even when they share an API origin.
func (c Config) SourceKey() string { return "twenty:" + c.APIOrigin + "/" + c.WorkspaceID }

// RemoteID identifies the authenticated workspace within the API origin.
func (c Config) RemoteID() string { return c.WorkspaceID }

// UseTitlePrefix defaults source-owned titles to the Twenty UUID prefix.
func (c Config) UseTitlePrefix() bool { return c.TitlePrefix == nil || *c.TitlePrefix }

// Workspace is the authenticated API key's source identity.
type Workspace struct{ ID, DisplayName string }

// Schema contains live API option values, independently of translated labels.
type Schema struct{ StatusOptions []string }

// Task is a complete, validated core API observation. Null status is open.
type Task struct {
	ID, Title, Markdown, CreatorID string
	Status, AssigneeID             *string
	CreatedAt, UpdatedAt           time.Time
}

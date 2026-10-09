// Package todoistsync imports scoped Todoist tasks and delivers verified status intent.
package todoistsync

import "time"

// Config pins the non-secret account, project, history floor and presentation policy.
type Config struct {
	APIOrigin    string `json:"api_origin"`
	AccountID    string `json:"account_id"`
	ProjectID    string `json:"project_id"`
	HistorySince string `json:"history_since"`
	StatusSync   string `json:"status_sync,omitempty"`
	TitlePrefix  *bool  `json:"title_prefix"`
}

// SourceKey separates task identities across API origins, accounts and projects.
func (c Config) SourceKey() string { return "todoist:" + c.APIOrigin + "/" + c.RemoteID() }

// RemoteID identifies the project within its credential account.
func (c Config) RemoteID() string { return c.AccountID + "/" + c.ProjectID }

// UseTitlePrefix defaults omitted presentation choices to prefixed titles.
func (c Config) UseTitlePrefix() bool { return c.TitlePrefix == nil || *c.TitlePrefix }

// Project is the selected Todoist project.
type Project struct {
	ID       string
	Name     string
	Archived bool
	Deleted  bool
}

// Task is the subset of a Todoist task that Kata imports and writes back.
type Task struct {
	ID          string
	ProjectID   string
	ParentID    string
	SectionID   string
	Content     string
	Description string
	AddedBy     string
	Assignee    string
	Labels      []string
	Priority    int
	AddedAt     time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
	Checked     bool
	Deleted     bool
	Recurring   bool
	// UpdatedAtUnknown records that Todoist reported updated_at as null, so
	// UpdatedAt holds a fallback that cannot order observations.
	UpdatedAtUnknown bool
}

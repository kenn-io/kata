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

// Project contains the selected source identity and archive/deletion evidence.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Archived *bool  `json:"is_archived"`
	Deleted  *bool  `json:"is_deleted"`
}

// Due contains the recurrence flag needed to prevent occurrence-advancing writes.
type Due struct {
	Recurring bool `json:"is_recurring"`
}

// Task follows the documented ItemSyncView, keeping absent status flags distinct.
type Task struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	ParentID    string     `json:"parent_id"`
	SectionID   string     `json:"section_id"`
	Content     string     `json:"content"`
	Description string     `json:"description"`
	AddedBy     string     `json:"added_by_uid"`
	Assignee    string     `json:"responsible_uid"`
	Labels      []string   `json:"labels"`
	Priority    int64      `json:"priority"`
	AddedAt     time.Time  `json:"added_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at"`
	Checked     *bool      `json:"checked"`
	Deleted     *bool      `json:"is_deleted"`
	Due         *Due       `json:"due"`
	// recurrenceKnown is ephemeral read evidence; absent fields never authorize a write.
	recurrenceKnown bool
	hierarchyKnown  bool
	updatedAtKnown  bool
}

// Package planesync maps read-only Plane project observations to native issues.
package planesync

import "time"

// Config is the canonical non-secret binding identity and presentation policy.
type Config struct {
	APIOrigin   string `json:"api_origin"`
	WebOrigin   string `json:"web_origin"`
	Workspace   string `json:"workspace"`
	ProjectID   string `json:"project_id"`
	Since       string `json:"since"`
	TitlePrefix *bool  `json:"title_prefix"`
}

// SourceKey separates work items from different Plane instances and projects.
func (c Config) SourceKey() string { return "plane:" + c.APIOrigin + "/" + c.RemoteID() }

// RemoteID identifies the project within the configured instance.
func (c Config) RemoteID() string { return c.Workspace + "/" + c.ProjectID }

// UseTitlePrefix defaults omitted presentation settings to prefixed titles.
func (c Config) UseTitlePrefix() bool { return c.TitlePrefix == nil || *c.TitlePrefix }

// Project contains the canonical source project and its display identity.
type Project struct{ ID, Name, Identifier string }

// State contains a canonical workflow identity and documented Plane group.
type State struct{ ID, Group string }

// WorkItem holds a complete unexpanded source observation.
type WorkItem struct {
	ID, ProjectID, StateID, Name, DescriptionHTML, CreatorID string
	SequenceID                                               int64
	AssigneeIDs                                              []string
	Priority                                                 *int64
	CreatedAt, UpdatedAt                                     time.Time
}

// Package linearsync imports scoped Linear issues and synchronizes binary status.
package linearsync

import "time"

// Config is the immutable source scope and non-secret presentation policy.
type Config struct {
	WorkspaceID   string `json:"workspace_id"`
	TeamID        string `json:"team_id"`
	ProjectID     string `json:"project_id,omitempty"`
	Since         string `json:"since"`
	StatusSync    string `json:"status_sync,omitempty"`
	ClosedStateID string `json:"closed_state_id,omitempty"`
	OpenStateID   string `json:"open_state_id,omitempty"`
	TitlePrefix   *bool  `json:"title_prefix"`
}

// RemoteID identifies the immutable workspace, team, and optional project scope.
func (c Config) RemoteID() string {
	id := c.WorkspaceID + "/" + c.TeamID
	if c.ProjectID != "" {
		id += "/" + c.ProjectID
	}
	return id
}

// SourceKey namespaces the stable import identity for Linear.
func (c Config) SourceKey() string { return "linear:" + c.RemoteID() }

// UseTitlePrefix reports the saved choice, defaulting to prefixed titles.
func (c Config) UseTitlePrefix() bool { return c.TitlePrefix == nil || *c.TitlePrefix }

// Scope is a verified organization/team/optional-project read.
type Scope struct{ WorkspaceID, TeamID, ProjectID, Name string }

// State is a live team workflow state, ordered within its type.
type State struct {
	ID, Type string
	Position float64
}

// Issue contains complete source fields; absent optional fields remain empty.
type Issue struct {
	ID, TeamID, ProjectID, StateID, Identifier, Title, Description, URL, CreatorID, AssigneeID string
	Priority                                                                                   int
	CreatedAt, UpdatedAt                                                                       time.Time
	CompletedAt, CanceledAt, ArchivedAt                                                        *time.Time
	Trashed                                                                                    bool
}

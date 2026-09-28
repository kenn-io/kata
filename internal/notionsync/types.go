// Package notionsync maps a Notion task data source to native imported issues.
package notionsync

import (
	"strings"
	"time"
)

// Config contains stable source and mapping identities and presentation choices.
// Property and option IDs are opaque: preserve their returned encoding.
type Config struct {
	DataSourceID       string   `json:"data_source_id"`
	DatabaseID         string   `json:"database_id"`
	TitlePropertyID    string   `json:"title_property_id"`
	StatusPropertyID   string   `json:"status_property_id"`
	AssigneePropertyID string   `json:"assignee_property_id"`
	DoneStatusIDs      []string `json:"done_status_ids"`
	Since              string   `json:"since"`
	TitlePrefix        *bool    `json:"title_prefix"`
}

// Selectors are initial, case-sensitive property and completion selections.
type Selectors struct {
	StatusProperty, AssigneeProperty string
	DoneStatuses                     []string
}

// Option identifies a status option or a child data source.
type Option struct{ ID, Name string }

// Property contains the source schema needed to validate the selected mapping.
type Property struct {
	ID, Name, Type string
	Options        []Option
}

// DataSource is the source identity, parent container, and current schema.
type DataSource struct {
	ID, DatabaseID, Name string
	Properties           []Property
}

// SourceDisplayName returns a stable non-empty label when Notion omits a source name.
func SourceDisplayName(source DataSource) string {
	if strings.TrimSpace(source.Name) != "" {
		return source.Name
	}
	if id := strings.TrimSpace(source.ID); id != "" {
		return "Notion data source " + id
	}
	return "Notion data source"
}

// Database contains the identities and names of its child data sources.
type Database struct {
	ID          string
	DataSources []Option
}

// Page is a validated, minimal provider observation, rather than an API wire row.
type Page struct {
	ID, URL, DataSourceID, DatabaseID, CreatorID string
	CreatedAt, UpdatedAt                         time.Time
	StatusID                                     *string
	IsArchived, InTrash                          bool
}

// PageContent holds complete title, markdown, and the first selected person ID.
type PageContent struct {
	Page            Page
	Title, Markdown string
	OwnerID         *string
}

// UseTitlePrefix defaults legacy bindings to prefixed titles.
func (c Config) UseTitlePrefix() bool {
	return c.TitlePrefix == nil || *c.TitlePrefix
}

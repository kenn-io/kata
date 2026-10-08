// Package tickticksync imports one TickTick project and delivers completion intent.
package tickticksync

import (
	"time"

	"go.kenn.io/kata/internal/db"
)

// Config contains only operator-selected non-secret settings.
type Config struct {
	ProjectID   string `json:"project_id"`
	StatusSync  string `json:"status_sync,omitempty"`
	TitlePrefix *bool  `json:"title_prefix"`
}

// SourceKey identifies the selected external project for durable mappings.
func (c Config) SourceKey() string { return "ticktick:" + c.ProjectID }

// RemoteID records the immutable provider project identity.
func (c Config) RemoteID() string { return c.ProjectID }

// UseTitlePrefix defaults to source-attributed issue titles.
func (c Config) UseTitlePrefix() bool { return c.TitlePrefix == nil || *c.TitlePrefix }

// Project records the source identity and live access policy.
type Project struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Closed     bool   `json:"closed"`
	Permission string `json:"permission"`
}

// Task contains the public fields imported into Kata; dates remain provider-owned.
type Task struct {
	ID            string          `json:"id"`
	ProjectID     string          `json:"projectId"`
	Title         string          `json:"title"`
	Content       string          `json:"content"`
	Desc          string          `json:"desc"`
	Status        *int            `json:"status"`
	Priority      int             `json:"priority"`
	Assignee      string          `json:"assigneeUsername"`
	Kind          string          `json:"kind"`
	RepeatFlag    string          `json:"repeatFlag"`
	StartDate     string          `json:"startDate"`
	DueDate       string          `json:"dueDate"`
	TimeZone      string          `json:"timeZone"`
	CompletedTime string          `json:"completedTime"`
	Items         []ChecklistItem `json:"items"`
}

// ChecklistItem is rendered inside the provider-owned body, never as another issue.
type ChecklistItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status int    `json:"status"`
}

// ProjectData is the unpaginated public project collection.
type ProjectData struct {
	Project Project `json:"project"`
	Tasks   []Task  `json:"tasks"`
}

const checkpointKey = db.IssueSyncProviderCheckpointKey
const maxTasks = 10000
const missingTaskLookupLimit = 100
const maxCheckpointBytes = 2 << 20

// TaskVersion checkpoints observations, not undocumented provider timestamps.
// Hash covers task content only; Status and CompletedTime record the last
// observed status separately so a status change never ages the content.
type TaskVersion struct {
	Hash          string    `json:"hash"`
	Status        int       `json:"status,omitzero"`
	CompletedTime string    `json:"completed_time,omitempty"`
	FirstSeen     time.Time `json:"first_seen"`
	Version       time.Time `json:"version"`
	// PendingRecovery marks a recovered task whose final import has not committed.
	PendingRecovery bool `json:"pending_recovery,omitzero"`
	// PendingStatus marks a one-way status observation whose import has not committed.
	PendingStatus bool `json:"pending_status,omitzero"`
}

// Checkpoint survives partial import/restart and bounds absent-task recovery laps.
type Checkpoint struct {
	Versions     map[string]TaskVersion `json:"versions"`
	MissingAfter string                 `json:"missing_after,omitempty"`
}

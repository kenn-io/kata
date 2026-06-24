package db

import (
	"encoding/json"
	"time"
)

// IssueSyncBinding mirrors issue_sync_bindings, the durable association
// between one kata project and one external issue provider source.
type IssueSyncBinding struct {
	ID              int64
	ProjectID       int64
	Provider        string
	SourceKey       string
	RemoteID        string
	DisplayName     string
	Config          json.RawMessage
	Enabled         bool
	IntervalSeconds int
	LastCursorAt    *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IssueSyncStatus records runner state and the latest observed sync outcome
// for one binding.
type IssueSyncStatus struct {
	BindingID     int64
	ProjectID     int64
	SyncStartedAt *time.Time
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastErrorAt   *time.Time
	LastError     string
	LastCreated   int
	LastUpdated   int
	LastUnchanged int
	LastComments  int
}

// UpsertIssueSyncBindingParams creates or re-enables the binding for a
// project. SourceKey is caller-derived, normally "<provider>:<remote_id>".
type UpsertIssueSyncBindingParams struct {
	ProjectID       int64
	Provider        string
	SourceKey       string
	RemoteID        string
	DisplayName     string
	Config          json.RawMessage
	IntervalSeconds int
}

// IssueSyncSuccessParams records a completed sync and advances the import
// cursor to CursorAt.
type IssueSyncSuccessParams struct {
	BindingID     int64
	StartedAt     time.Time
	At            time.Time
	CursorAt      time.Time
	LastCreated   int
	LastUpdated   int
	LastUnchanged int
	LastComments  int
}

// IssueSyncErrorParams records a failed sync attempt without advancing the
// import cursor.
type IssueSyncErrorParams struct {
	BindingID int64
	StartedAt time.Time
	At        time.Time
	Error     string
}

// IssueSyncBindingUpdateParams refreshes mutable provider-owned display/config
// fields while preserving the source key and stable remote id.
type IssueSyncBindingUpdateParams struct {
	BindingID   int64
	DisplayName string
	Config      json.RawMessage
}

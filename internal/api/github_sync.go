package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"

	"go.kenn.io/kata/internal/db"
)

// EnableIssueSyncRequest enables durable external issue sync for one project.
type EnableIssueSyncRequest struct {
	ProjectID int64  `path:"project_id"`
	Provider  string `path:"provider"`
	Body      EnableIssueSyncRequestBody
}

// EnableIssueSyncRequestBody carries provider-specific config and shared
// scheduling options. Config must not contain raw credentials; providers that
// need credentials should store references or use their own CLI auth.
type EnableIssueSyncRequestBody struct {
	StatusSync             *string `json:"status_sync,omitempty" enum:"one-way,two-way"`
	IntervalSecondsPresent bool    `json:"-"`
	Config                 JSONMap `json:"config,omitempty"`
	IntervalSeconds        int     `json:"interval_seconds,omitempty,omitzero"`
	Interval               string  `json:"interval,omitempty"`
}

// UnmarshalJSON preserves presence without changing the public integer field or schema.
func (b *EnableIssueSyncRequestBody) UnmarshalJSON(data []byte) error {
	type body EnableIssueSyncRequestBody
	var decoded body
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var members map[string]jsontext.Value
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	_, decoded.IntervalSecondsPresent = members["interval_seconds"]
	*b = EnableIssueSyncRequestBody(decoded)
	return nil
}

// DisableIssueSyncRequest disables durable external issue sync for one project.
type DisableIssueSyncRequest struct {
	ProjectID int64  `path:"project_id"`
	Provider  string `path:"provider"`
	Body      DisableIssueSyncRequestBody
}

// DisableIssueSyncRequestBody is the intentionally empty request payload.
type DisableIssueSyncRequestBody struct{}

// IssueSyncStatusRequest reads durable external issue sync state for one project.
type IssueSyncStatusRequest struct {
	ProjectID int64  `path:"project_id"`
	Provider  string `path:"provider"`
}

// RunIssueSyncOnceRequest runs one immediate daemon-side issue sync.
type RunIssueSyncOnceRequest struct {
	ProjectID int64  `path:"project_id"`
	Provider  string `path:"provider"`
	Body      RunIssueSyncOnceRequestBody
}

// RunIssueSyncOnceRequestBody is the intentionally empty request payload.
// Keep it distinct from RunIssueSyncOnceResponseBody: oapi-codegen keys the
// generated request-options Body field off the operation request schema.
type RunIssueSyncOnceRequestBody struct{}

// IssueSyncBindingOut is the API-owned representation of one sync binding.
type IssueSyncBindingOut struct {
	ID              int64      `json:"id"`
	ProjectID       int64      `json:"project_id"`
	Provider        string     `json:"provider"`
	SourceKey       string     `json:"source_key"`
	RemoteID        string     `json:"remote_id"`
	DisplayName     string     `json:"display_name"`
	Config          JSONMap    `json:"config,omitempty"`
	Enabled         bool       `json:"enabled"`
	IntervalSeconds int        `json:"interval_seconds"`
	LastCursorAt    *time.Time `json:"last_cursor_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// IssueSyncStatusOut summarizes current sync state.
type IssueSyncStatusOut struct {
	StatusSync    string                `json:"status_sync"`
	PendingCount  int                   `json:"pending_count"`
	Progress      *IssueSyncProgressOut `json:"progress,omitempty"`
	BindingID     int64                 `json:"binding_id"`
	ProjectID     int64                 `json:"project_id"`
	Provider      string                `json:"provider"`
	Enabled       bool                  `json:"enabled"`
	State         string                `json:"state"`
	SyncStartedAt *time.Time            `json:"sync_started_at,omitempty"`
	LastAttemptAt *time.Time            `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time            `json:"last_success_at,omitempty"`
	LastErrorAt   *time.Time            `json:"last_error_at,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
	LastCreated   int                   `json:"last_created"`
	LastUpdated   int                   `json:"last_updated"`
	LastUnchanged int                   `json:"last_unchanged"`
	LastComments  int                   `json:"last_comments"`
}

// IssueSyncBody is the shared response envelope for enable, disable, and status.
type IssueSyncBody struct {
	Binding *IssueSyncBindingOut `json:"binding,omitempty"`
	Status  IssueSyncStatusOut   `json:"status"`
}

// IssueSyncResponse wraps IssueSyncBody.
type IssueSyncResponse struct {
	Body IssueSyncBody
}

// RunIssueSyncOnceResponseBody extends the normal sync body with the import summary.
type RunIssueSyncOnceResponseBody struct {
	StatusUpdated int                  `json:"status_updated"`
	Binding       *IssueSyncBindingOut `json:"binding"`
	Status        IssueSyncStatusOut   `json:"status"`
	Import        db.ImportBatchResult `json:"import"`
}

// RunIssueSyncOnceResponse wraps RunIssueSyncOnceResponseBody.
type RunIssueSyncOnceResponse struct {
	Body RunIssueSyncOnceResponseBody
}

// DecodeJSONMap decodes a durable provider config blob into an API object.
func DecodeJSONMap(raw jsontext.Value) (JSONMap, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out JSONMap
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// IssueSyncProgressOut describes live phase-local work for the active sync claim.
// Total is zero when no reliable total is available.
type IssueSyncProgressOut struct {
	Phase     string    `json:"phase"`
	Completed int       `json:"completed"`
	Total     int       `json:"total"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

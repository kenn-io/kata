package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json"
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
	Config          map[string]any `json:"config,omitempty"`
	IntervalSeconds int            `json:"interval_seconds,omitempty"`
	Interval        string         `json:"interval,omitempty"`
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

// JSONMap is an opaque JSON object used for provider-specific issue-sync
// configuration in API responses.
type JSONMap map[string]any

// IssueSyncStatusOut summarizes current sync state.
type IssueSyncStatusOut struct {
	BindingID     int64      `json:"binding_id"`
	ProjectID     int64      `json:"project_id"`
	Provider      string     `json:"provider"`
	Enabled       bool       `json:"enabled"`
	State         string     `json:"state"`
	SyncStartedAt *time.Time `json:"sync_started_at,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastCreated   int        `json:"last_created"`
	LastUpdated   int        `json:"last_updated"`
	LastUnchanged int        `json:"last_unchanged"`
	LastComments  int        `json:"last_comments"`
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
	Binding *IssueSyncBindingOut `json:"binding"`
	Status  IssueSyncStatusOut   `json:"status"`
	Import  db.ImportBatchResult `json:"import"`
}

// RunIssueSyncOnceResponse wraps RunIssueSyncOnceResponseBody.
type RunIssueSyncOnceResponse struct {
	Body RunIssueSyncOnceResponseBody
}

// DecodeJSONMap decodes a durable provider config blob into an API object.
func DecodeJSONMap(raw json.RawMessage) (JSONMap, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out JSONMap
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

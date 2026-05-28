// Package api types for Plan 4 events endpoints. EventEnvelope is the JSON
// shape carried in SSE data: lines and the events array of PollEventsResponse;
// it mirrors db.Event one-for-one but lives in api so the wire schema stays
// independent of internal storage shape.
package api //nolint:revive // package name "api" is fixed by Plan 1 §4 wire-types layout.

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// OptionalInt is a Huma-compatible query param wrapper for optional integers.
// It tracks whether the param was explicitly set (as opposed to absent/zero).
// Implements huma.SchemaProvider (schema appears as integer), huma.ParamWrapper
// (Huma parses into Value), and huma.ParamReactor (records whether provided).
type OptionalInt struct {
	Value int
	IsSet bool
}

// Schema implements huma.SchemaProvider so the OpenAPI schema is "integer"
// rather than the default struct schema.
func (OptionalInt) Schema(r huma.Registry) *huma.Schema {
	return huma.SchemaFromType(r, reflect.TypeOf(0))
}

// Receiver returns the reflect.Value of the int field for Huma to parse into.
func (o *OptionalInt) Receiver() reflect.Value {
	return reflect.ValueOf(o).Elem().Field(0)
}

// OnParamSet records whether the query param was present in the request.
func (o *OptionalInt) OnParamSet(isSet bool, _ any) {
	o.IsSet = isSet
}

// EventEnvelope is the wire shape for a single event row. IssueUID is the
// canonical reference; IssueShortID is the rendered display value the daemon
// joins from the live issues table at response time (so old events render
// correctly across cutovers and federation merges).
type EventEnvelope struct {
	EventID             int64   `json:"event_id"`
	EventUID            string  `json:"event_uid"`
	OriginInstanceUID   string  `json:"origin_instance_uid"`
	Type                string  `json:"type"`
	ProjectID           int64   `json:"project_id"`
	ProjectUID          string  `json:"project_uid"`
	ProjectName         string  `json:"project_name"`
	IssueID             *int64  `json:"issue_id,omitempty"`
	IssueUID            *string `json:"issue_uid,omitempty"`
	IssueShortID        *string `json:"issue_short_id,omitempty"`
	RelatedIssueID      *int64  `json:"related_issue_id,omitempty"`
	RelatedIssueUID     *string `json:"related_issue_uid,omitempty"`
	RelatedIssueShortID *string `json:"related_issue_short_id,omitempty"`
	Actor               string  `json:"actor"`
	HLCPhysicalMS       int64   `json:"hlc_physical_ms"`
	HLCCounter          int64   `json:"hlc_counter"`
	ContentHash         string  `json:"content_hash"`
	// Payload is the event-type-specific JSON object. Always valid JSON
	// because the schema enforces json_valid(payload) at write time.
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// EventReset is the data: payload of a sync.reset_required SSE frame and the
// stripped-down content of a poll response when the cursor falls inside a
// purge gap.
type EventReset struct {
	EventID      int64 `json:"event_id"`       // == ResetAfterID; mirrors the SSE id: line.
	ResetAfterID int64 `json:"reset_after_id"` // minimum safe resume cursor.
}

// PollEventsRequest is GET /api/v1/projects/{project_id}/events (per-project).
// AfterID is exclusive; the response's NextAfterID is the cursor the client
// should pass on the next request. Limit defaults to 100 and is clamped to
// 1000 server-side; explicit zero or negative Limit returns 400 validation.
type PollEventsRequest struct {
	ProjectID int64       `path:"project_id"`
	AfterID   int64       `query:"after_id,omitempty"`
	Limit     OptionalInt `query:"limit,omitempty"`
}

// PollEventsGlobalRequest is GET /api/v1/events (cross-project). Same semantics
// as PollEventsRequest but without the path-level project_id, which Huma
// marks required and would reject requests to the global endpoint.
type PollEventsGlobalRequest struct {
	AfterID int64       `query:"after_id,omitempty"`
	Limit   OptionalInt `query:"limit,omitempty"`
}

// PollEventsBody is the response body for both polling endpoints.
type PollEventsBody struct {
	ResetRequired bool            `json:"reset_required"`
	ResetAfterID  int64           `json:"reset_after_id,omitempty"`
	Events        []EventEnvelope `json:"events"`        // always non-nil; empty array on no rows
	NextAfterID   int64           `json:"next_after_id"` // = max events.id in response, or after_id if empty
}

// PollEventsResponse is the response for both polling endpoints. ResetRequired
// signals a purge-invalidated cursor; when true, Events is empty and the
// client should refetch state and resume from ResetAfterID.
type PollEventsResponse struct {
	Body PollEventsBody
}

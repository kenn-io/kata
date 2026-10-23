// Package mcpevents owns the daemon's durable MCP Events subscriptions.
package mcpevents

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"time"
)

// EventName is the only event type the daemon serves.
const EventName = "kata.inbox.changed"

// Resource limits for requests and payloads.
const (
	MaxProjects      = 128
	MaxEnvelopeBytes = 256 << 10
)

// Arguments identify an exact recipient and an optional durable scope.
// ProjectUIDs nil means every project the principal can see, now and later.
// When present it must be non-empty, sorted ascending and free of duplicates.
type Arguments struct {
	For         string   `json:"for"`
	ProjectUIDs []string `json:"project_uids,omitempty"`
	IssueUID    string   `json:"issue_uid,omitempty"`
}

// Dynamic reports whether the selection follows the principal's catalog.
func (a Arguments) Dynamic() bool { return a.ProjectUIDs == nil }

// Delivery selects a signed webhook callback and its signing secret.
type Delivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

// TTLMS is the tri-state ttlMs request field: absent, null (no expiry), or a
// finite suggestion in milliseconds. The zero value is absent.
type TTLMS struct {
	set, null bool
	value     int64
}

// TTLNull requests a subscription with no expiry.
func TTLNull() TTLMS { return TTLMS{set: true, null: true} }

// TTLValue suggests a finite lifetime in milliseconds.
func TTLValue(ms int64) TTLMS { return TTLMS{set: true, value: ms} }

// Present reports whether the field was supplied, as a value or as null.
func (t TTLMS) Present() bool { return t.set }

// IsNull reports an explicit null.
func (t TTLMS) IsNull() bool { return t.set && t.null }

// Millis reports the field as a forwarder sends it to the daemon: a finite
// suggestion, or an explicit no-expiry request. It inverts TTLFromWire.
func (t TTLMS) Millis() (ms *int64, noExpiry bool) {
	switch {
	case !t.set:
		return nil, false
	case t.null:
		return nil, true
	default:
		value := t.value
		return &value, false
	}
}

// UnmarshalJSON accepts an integer or null.
func (t *TTLMS) UnmarshalJSON(data []byte) error {
	if string(jsontext.Value(data)) == "null" {
		*t = TTLMS{set: true, null: true}
		return nil
	}
	var ms int64
	if err := json.Unmarshal(data, &ms); err != nil {
		return err
	}
	*t = TTLMS{set: true, value: ms}
	return nil
}

// MarshalJSON renders null for an explicit null and the integer otherwise.
func (t TTLMS) MarshalJSON() ([]byte, error) {
	if !t.set || t.null {
		return []byte("null"), nil
	}
	return json.Marshal(t.value)
}

// IsZero lets omitzero drop an absent field.
func (t TTLMS) IsZero() bool { return !t.set }

// SubscribeRequest registers or refreshes a webhook subscription.
type SubscribeRequest struct {
	Name      string    `json:"name"`
	Arguments Arguments `json:"arguments"`
	Delivery  Delivery  `json:"delivery"`
	Cursor    *string   `json:"cursor,omitempty"`
	TTLMS     TTLMS     `json:"ttlMs,omitzero"`
	MaxAgeMS  *int64    `json:"maxAgeMs,omitempty"`
}

// SubscribeResult returns the subscription handle, its lease and a safe cursor.
type SubscribeResult struct {
	ID             string         `json:"id"`
	RefreshBefore  *time.Time     `json:"refreshBefore"`
	Cursor         string         `json:"cursor"`
	Truncated      bool           `json:"truncated"`
	DeliveryStatus DeliveryStatus `json:"deliveryStatus"`
}

// DeliveryStatus is the SEP-3415 partial delivery status. LastError is a
// failure category, never a response, URL or transport error.
type DeliveryStatus struct {
	Active         bool       `json:"active"`
	LastDeliveryAt *time.Time `json:"lastDeliveryAt,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	FailedSince    *time.Time `json:"failedSince,omitempty"`
}

// UnsubscribeRequest identifies the subscription to end.
type UnsubscribeRequest struct {
	Name      string    `json:"name"`
	Arguments Arguments `json:"arguments"`
	Delivery  Delivery  `json:"delivery"`
}

// ForwarderScope is what the calling `kata mcp serve` forwarder serves. All is
// set only for the all-projects mode.
type ForwarderScope struct {
	All      bool
	Projects []string
}

// Payload carries safe inbox attention references without notification text.
type Payload struct {
	Recipient      string `json:"recipient"`
	Reason         string `json:"reason"`
	ProjectUID     string `json:"project_uid,omitempty"`
	IssueUID       string `json:"issue_uid,omitempty"`
	SourceEventUID string `json:"source_event_uid,omitempty"`
}

// Envelope is the signed native Events occurrence sent to a callback.
type Envelope struct {
	EventID   string  `json:"eventId"`
	Name      string  `json:"name"`
	Timestamp string  `json:"timestamp"`
	Data      Payload `json:"data"`
	Cursor    string  `json:"cursor"`
}

// Definition advertises an event and its argument and payload schemas.
type Definition struct {
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Delivery      []string       `json:"delivery"`
	InputSchema   map[string]any `json:"inputSchema"`
	PayloadSchema map[string]any `json:"payloadSchema"`
}

// ListResult contains the native event definitions available to a principal.
type ListResult struct {
	Events []Definition `json:"events"`
}

// SubscriptionStatus excludes destinations, keys and envelope contents. The
// recipient and selection let a forwarder filter its display.
type SubscriptionStatus struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Recipient            string         `json:"recipient"`
	ProjectUIDs          []string       `json:"project_uids,omitempty"`
	IssueUID             string         `json:"issue_uid,omitempty"`
	State                string         `json:"state"`
	EndReason            string         `json:"end_reason,omitempty"`
	RefreshBefore        *time.Time     `json:"refreshBefore"`
	Cursor               string         `json:"cursor"`
	ContinuityGeneration int64          `json:"continuity_generation"`
	GapUnacked           bool           `json:"gap_unacked"`
	Delivery             DeliveryStatus `json:"delivery"`
	FailedAttempts       int            `json:"failed_attempts"`
	DeadLetterCount      int            `json:"dead_letter_count"`
	PendingControl       string         `json:"pending_control,omitempty"`
}

// definition is the single advertised event type.
func definition() Definition {
	return Definition{
		Name:        EventName,
		Description: "A request addressed to one inbox recipient changed, or visibility of a related issue changed. Payloads carry identifiers only; read kata.inbox for content.",
		Delivery:    []string{"webhook"},
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"for":          map[string]any{"type": "string", "description": "Exact inbox recipient."},
				"project_uids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "uniqueItems": true, "description": "Optional sorted ascending project UIDs; omit to follow every visible project."},
				"issue_uid":    map[string]any{"type": "string", "description": "Optional issue UID filter."},
			},
			"required":             []string{"for"},
			"additionalProperties": false,
		},
		PayloadSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"recipient":        map[string]any{"type": "string"},
				"reason":           map[string]any{"type": "string", "enum": []string{"notification", "visibility"}},
				"project_uid":      map[string]any{"type": "string"},
				"issue_uid":        map[string]any{"type": "string"},
				"source_event_uid": map[string]any{"type": "string"},
			},
			"required": []string{"recipient", "reason"},
		},
	}
}

var errNoStore = errors.New("MCP Events service needs a store")

// maxForwarderProjects bounds a forwarder's served-project list.
const maxForwarderProjects = 1024

// NewForwarderScope validates the scope a forwarder reports. It is advisory
// narrowing only, so an invalid scope is rejected and never repaired. All and
// a project list are mutually exclusive, and a non-All scope needs at least
// one canonical project UID, without duplicates.
func NewForwarderScope(all bool, projects []string) (ForwarderScope, error) {
	const reason = "invalid_forwarder_scope"
	if all {
		if len(projects) > 0 {
			return ForwarderScope{}, invalidParams(reason)
		}
		return ForwarderScope{All: true}, nil
	}
	if len(projects) == 0 || len(projects) > maxForwarderProjects {
		return ForwarderScope{}, invalidParams(reason)
	}
	seen := make(map[string]bool, len(projects))
	for _, uid := range projects {
		if !canonicalULID(uid) || seen[uid] {
			return ForwarderScope{}, invalidParams(reason)
		}
		seen[uid] = true
	}
	return ForwarderScope{Projects: append([]string(nil), projects...)}, nil
}

// TTLFromWire builds the tri-state ttlMs from a finite suggestion and an
// explicit no-expiry request. Both together contradict each other.
func TTLFromWire(ms *int64, noExpiry bool) (TTLMS, error) {
	switch {
	case noExpiry && ms != nil:
		return TTLMS{}, invalidParams("ttl_conflict")
	case noExpiry:
		return TTLNull(), nil
	case ms != nil:
		return TTLValue(*ms), nil
	default:
		return TTLMS{}, nil
	}
}

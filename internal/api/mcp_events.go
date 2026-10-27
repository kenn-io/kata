package api

import "time"

// MCPEventsArguments identify an exact inbox recipient and an optional durable
// project selection. ProjectUIDs omitted means every project the principal can
// see, now and later; when present it must be non-empty, sorted ascending and
// free of duplicates.
type MCPEventsArguments struct {
	For         string   `json:"for"`
	ProjectUIDs []string `json:"project_uids,omitzero"`
	IssueUID    string   `json:"issue_uid,omitempty"`
}

// MCPEventsDelivery selects a signed webhook callback. Secret is required to
// subscribe and ignored to unsubscribe. It is never returned.
type MCPEventsDelivery struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

// MCPEventsForwarderScope is what the calling forwarder serves. It only
// narrows: it can never widen what the authenticated principal may see.
// Omitting it applies no narrowing. All and Projects are mutually exclusive.
type MCPEventsForwarderScope struct {
	All      bool     `json:"all,omitzero"`
	Projects []string `json:"projects,omitzero"`
}

// MCPEventsEventDefinition advertises one event type.
type MCPEventsEventDefinition struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Delivery      []string `json:"delivery"`
	InputSchema   JSONMap  `json:"input_schema"`
	PayloadSchema JSONMap  `json:"payload_schema"`
}

// MCPEventsListResponse returns the event definitions available to the caller.
type MCPEventsListResponse struct {
	Body struct {
		Events []MCPEventsEventDefinition `json:"events"`
	}
}

// MCPEventsSubscribeBody registers or refreshes a webhook subscription. TTLMS
// absent means the 24 hour default; NoExpiry requests no expiry and cannot be
// combined with TTLMS. MaxAgeMS applies only when resuming from a Cursor.
type MCPEventsSubscribeBody struct {
	Name           string                   `json:"name"`
	Arguments      MCPEventsArguments       `json:"arguments"`
	Delivery       MCPEventsDelivery        `json:"delivery"`
	Cursor         *string                  `json:"cursor,omitempty"`
	TTLMS          *int64                   `json:"ttl_ms,omitempty"`
	NoExpiry       bool                     `json:"no_expiry,omitzero"`
	MaxAgeMS       *int64                   `json:"max_age_ms,omitempty"`
	ForwarderScope *MCPEventsForwarderScope `json:"forwarder_scope,omitempty"`
}

// MCPEventsSubscribeRequest carries the subscribe body.
type MCPEventsSubscribeRequest struct {
	Body MCPEventsSubscribeBody
}

// MCPEventsDeliveryStatus is a failure-category-only delivery summary.
type MCPEventsDeliveryStatus struct {
	Active         bool       `json:"active"`
	LastDeliveryAt *time.Time `json:"last_delivery_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	FailedSince    *time.Time `json:"failed_since,omitempty"`
}

// MCPEventsSubscribeResponse returns the handle, lease and a safe cursor.
type MCPEventsSubscribeResponse struct {
	Body struct {
		ID             string                  `json:"id"`
		RefreshBefore  *time.Time              `json:"refresh_before" nullable:"true"`
		Cursor         string                  `json:"cursor"`
		Truncated      bool                    `json:"truncated"`
		DeliveryStatus MCPEventsDeliveryStatus `json:"delivery_status"`
	}
}

// MCPEventsUnsubscribeBody identifies the subscription to end.
type MCPEventsUnsubscribeBody struct {
	Name      string             `json:"name"`
	Arguments MCPEventsArguments `json:"arguments"`
	Delivery  MCPEventsDelivery  `json:"delivery"`
}

// MCPEventsUnsubscribeRequest carries the unsubscribe body.
type MCPEventsUnsubscribeRequest struct {
	Body MCPEventsUnsubscribeBody
}

// MCPEventsUnsubscribeResponse confirms the subscription ended.
type MCPEventsUnsubscribeResponse struct {
	Body struct {
		Unsubscribed bool `json:"unsubscribed"`
	}
}

// MCPEventsSubscriptionStatus excludes destinations, keys and envelope
// contents. The recipient and selection let a forwarder filter its display.
type MCPEventsSubscriptionStatus struct {
	ID                   string                  `json:"id"`
	Name                 string                  `json:"name"`
	Recipient            string                  `json:"recipient"`
	ProjectUIDs          []string                `json:"project_uids,omitzero"`
	IssueUID             string                  `json:"issue_uid,omitempty"`
	State                string                  `json:"state"`
	EndReason            string                  `json:"end_reason,omitempty"`
	RefreshBefore        *time.Time              `json:"refresh_before" nullable:"true"`
	Cursor               string                  `json:"cursor"`
	ContinuityGeneration int64                   `json:"continuity_generation"`
	GapUnacked           bool                    `json:"gap_unacked"`
	Delivery             MCPEventsDeliveryStatus `json:"delivery"`
	FailedAttempts       int                     `json:"failed_attempts"`
	DeadLetterCount      int                     `json:"dead_letter_count"`
	PendingControl       string                  `json:"pending_control,omitempty"`
}

// MCPEventsSubscriptionsResponse lists the caller's subscriptions. When Events
// is disabled, Enabled is false, Reason says why and the list is empty.
type MCPEventsSubscriptionsResponse struct {
	Body struct {
		Enabled       bool                          `json:"enabled"`
		Reason        string                        `json:"reason,omitempty"`
		Subscriptions []MCPEventsSubscriptionStatus `json:"subscriptions"`
	}
}

// Keys of the structured data an MCP Events error carries: the §7.2 wire code
// and its data object, so a forwarder can reproduce the protocol error.
const (
	MCPEventsErrorCodeKey = "mcp_code"
	MCPEventsErrorDataKey = "mcp_data"
)

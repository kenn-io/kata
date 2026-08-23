// Package connector defines the versioned wire contract for external root connectors.
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ProtocolVersion is the exact connector wire-protocol version.
const ProtocolVersion = "kata.connector.v1"

// Capability names an optional connector operation.
type Capability string

const (
	// CapabilityPublishComment permits creating external comments.
	CapabilityPublishComment Capability = "publish_comment"
	// CapabilityFields permits reading and writing external fields.
	CapabilityFields Capability = "fields"
)

// Request is one versioned connector invocation.
type Request struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Instance string          `json:"instance"`
	Settings json.RawMessage `json:"settings,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// Invocation identifies the configured connector instance and settings for a
// handler call. Settings is a copy of the request's raw JSON settings.
type Invocation struct {
	Instance string
	Settings json.RawMessage
}

type invocationContextKey struct{}

type invocationContextValue struct {
	instance string
	settings string
}

// InvocationFromContext returns a copy of the invocation attached by ServeOne
// for a handler call.
func InvocationFromContext(ctx context.Context) (Invocation, bool) {
	stored, ok := ctx.Value(invocationContextKey{}).(invocationContextValue)
	if !ok {
		return Invocation{}, false
	}
	return Invocation{
		Instance: stored.instance,
		Settings: json.RawMessage(stored.settings),
	}, true
}

func withInvocation(ctx context.Context, request Request) context.Context {
	return context.WithValue(ctx, invocationContextKey{}, invocationContextValue{
		instance: request.Instance,
		settings: string(request.Settings),
	})
}

// Response is one versioned connector result or structured error.
type Response struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

// Error is a connector-supplied structured protocol error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// Description identifies a connector instance and its current capabilities.
type Description struct {
	ConnectorID     string          `json:"connector_id"`
	DisplayName     string          `json:"display_name"`
	Protocol        string          `json:"protocol"`
	Capabilities    []Capability    `json:"capabilities"`
	ConfigSchema    json.RawMessage `json:"config_schema,omitempty"`
	SelfActorID     string          `json:"self_actor_id,omitempty"`
	AccountIdentity string          `json:"account_identity"`
}

// FieldDescriptor describes one externally addressable field.
type FieldDescriptor struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name"`
	AcceptedKinds  []string `json:"accepted_kinds"`
	Nullable       bool     `json:"nullable"`
	Writable       bool     `json:"writable"`
	SchemaRevision string   `json:"schema_revision"`
}

// Root is the connector's canonical observation of one external root.
type Root struct {
	Key         string                `json:"key"`
	IdentityKey string                `json:"identity_key"`
	Title       string                `json:"title"`
	Body        string                `json:"body"`
	State       string                `json:"state"`
	Revision    string                `json:"revision"`
	UpdatedAt   time.Time             `json:"updated_at"`
	ObservedAt  time.Time             `json:"observed_at"`
	Actor       *Actor                `json:"actor,omitempty"`
	Fields      map[string]FieldValue `json:"fields,omitempty"`
}

// Actor identifies an external author without exposing provider credentials.
type Actor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Comment is one external-root comment observation.
type Comment struct {
	ID        string    `json:"id"`
	Revision  string    `json:"revision"`
	Body      string    `json:"body"`
	Author    Actor     `json:"author"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Deleted   bool      `json:"deleted"`
}

// FieldValue is a provider-neutral canonical field value.
type FieldValue struct {
	Kind     string `json:"kind"`
	Value    string `json:"value,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

// ValidateFieldValue rejects values outside the canonical portable field
// representation used by ProtocolVersion.
func ValidateFieldValue(value FieldValue) error {
	switch value.Kind {
	case "null":
		if value.Value != "" || value.Timezone != "" {
			return errors.New("null field value contains a value")
		}
	case "date":
		parsed, err := time.Parse("2006-01-02", value.Value)
		if err != nil || parsed.Format("2006-01-02") != value.Value || value.Timezone != "" {
			return errors.New("field value is not a canonical date")
		}
	case "instant":
		parsed, err := time.Parse(time.RFC3339Nano, value.Value)
		if err != nil || parsed.UTC().Format(time.RFC3339Nano) != value.Value || value.Timezone != "" {
			return errors.New("field value is not a canonical UTC instant")
		}
	case "local_datetime":
		parsed, err := time.Parse("2006-01-02T15:04:05", value.Value)
		location, zoneErr := time.LoadLocation(value.Timezone)
		if err != nil || parsed.Format("2006-01-02T15:04:05") != value.Value ||
			zoneErr != nil || value.Timezone == "Local" || location.String() != value.Timezone {
			return errors.New("local datetime field value is incomplete")
		}
	default:
		return errors.New("field value has unsupported kind")
	}
	return nil
}

// DescribeParams carries the empty describe request.
type DescribeParams struct{}

// ResolveRootParams locates an external root from user input.
type ResolveRootParams struct {
	Locator string `json:"locator"`
}

// ReadRootParams identifies an external root by its stable key.
type ReadRootParams struct {
	RootKey string `json:"root_key"`
}

// ListCommentsParams identifies the external root whose comments are requested.
type ListCommentsParams struct {
	RootKey string `json:"root_key"`
}

// ListCommentsResult contains the current external comment observations.
type ListCommentsResult struct {
	Comments []Comment `json:"comments"`
}

// CompleteRootParams identifies the external root to complete.
type CompleteRootParams struct {
	RootKey string `json:"root_key"`
}

// PublishCommentParams creates a comment on an external root.
type PublishCommentParams struct {
	RootKey     string `json:"root_key"`
	Body        string `json:"body"`
	OperationID string `json:"operation_id"`
}

// ValidOperationID reports whether value is a canonical publication idempotency key.
func ValidOperationID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

// ListFieldsParams carries the empty field-discovery request.
type ListFieldsParams struct{}

// ListFieldsResult contains the connector's current field descriptors.
type ListFieldsResult struct {
	Fields []FieldDescriptor `json:"fields"`
}

// ReadFieldsParams selects fields to read from an external root.
type ReadFieldsParams struct {
	RootKey  string   `json:"root_key"`
	FieldIDs []string `json:"field_ids"`
}

// ReadFieldsResult contains canonical values keyed by external field ID.
type ReadFieldsResult struct {
	Fields map[string]FieldValue `json:"fields"`
}

// WriteFieldsParams applies canonical field values to an external root.
type WriteFieldsParams struct {
	RootKey string                `json:"root_key"`
	Fields  map[string]FieldValue `json:"fields"`
}

// WriteFieldsResult contains the connector's canonical readback after writing.
type WriteFieldsResult struct {
	Fields map[string]FieldValue `json:"fields"`
}

// Handler implements all methods in ProtocolVersion.
type Handler interface {
	Describe(context.Context, DescribeParams) (Description, *Error)
	ResolveRoot(context.Context, ResolveRootParams) (Root, *Error)
	ReadRoot(context.Context, ReadRootParams) (Root, *Error)
	ListComments(context.Context, ListCommentsParams) (ListCommentsResult, *Error)
	CompleteRoot(context.Context, CompleteRootParams) (Root, *Error)
	PublishComment(context.Context, PublishCommentParams) (Comment, *Error)
	ListFields(context.Context, ListFieldsParams) (ListFieldsResult, *Error)
	ReadFields(context.Context, ReadFieldsParams) (ReadFieldsResult, *Error)
	WriteFields(context.Context, WriteFieldsParams) (WriteFieldsResult, *Error)
}

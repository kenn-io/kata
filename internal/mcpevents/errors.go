package mcpevents

import "errors"

// Wire error codes (design spec §7.2). The numbers are provisional upstream, so
// this table is the only place that spells them.
const (
	// CodeHeaderMismatch rejects a routing header that disagrees with the body.
	CodeHeaderMismatch = -32020
	// CodeNotFound reports an unknown event or subscription.
	CodeNotFound = -32023
	// CodeForbidden reports missing authority.
	CodeForbidden = -32024
	// CodeResourceExhausted reports an exceeded quota.
	CodeResourceExhausted = -32025
	// CodeUnsupported reports an unsupported delivery mode.
	CodeUnsupported = -32026
	// CodeCallbackEndpoint reports a failed endpoint verification.
	CodeCallbackEndpoint = -32027
	// CodeInvalidParams reports a malformed request.
	CodeInvalidParams = -32602
	// CodeUnavailable is an implementation-range code (-32000..-32019) for
	// conditions the protocol does not name, such as a disabled service.
	CodeUnavailable = -32010
)

// WireName returns the SEP-3415 name of a wire code, which is the JSON-RPC
// error message. Implementation-range and unknown codes are "Unavailable".
func WireName(code int) string {
	switch code {
	case CodeInvalidParams:
		return "InvalidParams"
	case CodeNotFound:
		return "NotFound"
	case CodeForbidden:
		return "Forbidden"
	case CodeResourceExhausted:
		return "ResourceExhausted"
	case CodeUnsupported:
		return "Unsupported"
	case CodeCallbackEndpoint:
		return "CallbackEndpointError"
	case CodeHeaderMismatch:
		return "HeaderMismatch"
	default:
		return "Unavailable"
	}
}

// Error carries only a fixed protocol code and a safe categorized reason.
// Reason is the primary data value: the reason, kind, limit or feature.
type Error struct {
	Code   int            `json:"code"`
	Reason string         `json:"reason"`
	Data   map[string]any `json:"data,omitempty"`
}

func (e *Error) Error() string { return "MCP Events: " + e.Reason }

// Is matches any Error with the same code, so the Err* sentinels below work
// with errors.Is regardless of the reason.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinels for errors.Is checks against a wire error class.
var (
	ErrForbidden     = &Error{Code: CodeForbidden, Reason: "forbidden"}
	ErrInvalidParams = &Error{Code: CodeInvalidParams, Reason: "invalid_params"}
	ErrNotFound      = &Error{Code: CodeNotFound, Reason: "not_found"}
	ErrExhausted     = &Error{Code: CodeResourceExhausted, Reason: "resource_exhausted"}
	ErrUnsupported   = &Error{Code: CodeUnsupported, Reason: "unsupported"}
	ErrCallback      = &Error{Code: CodeCallbackEndpoint, Reason: "callback_endpoint"}
	ErrDisabled      = &Error{Code: CodeUnavailable, Reason: "unavailable"}
)

func invalidParams(reason string) *Error {
	return &Error{Code: CodeInvalidParams, Reason: reason, Data: map[string]any{"reason": reason}}
}

func notFound(kind string) *Error {
	return &Error{Code: CodeNotFound, Reason: kind, Data: map[string]any{"kind": kind}}
}

func forbidden(reason string) *Error {
	return &Error{Code: CodeForbidden, Reason: reason, Data: map[string]any{"reason": reason}}
}

func resourceExhausted(limit string, maximum int) *Error {
	return &Error{Code: CodeResourceExhausted, Reason: limit, Data: map[string]any{"limit": limit, "max": maximum}}
}

func unsupported(feature string) *Error {
	return &Error{Code: CodeUnsupported, Reason: feature, Data: map[string]any{"feature": feature}}
}

func callbackEndpoint(category string) *Error {
	return &Error{Code: CodeCallbackEndpoint, Reason: category, Data: map[string]any{"reason": category}}
}

func unavailable(reason string) *Error {
	return &Error{Code: CodeUnavailable, Reason: reason, Data: map[string]any{"reason": reason}}
}

// Source errors distinguish revocation, transient failures, and visibility changes.
var (
	ErrRevoked          = errors.New("event source authority revoked")
	ErrUnavailable      = errors.New("event source temporarily unavailable")
	ErrOccurrenceHidden = errors.New("event occurrence is outside current source visibility")
	// ErrScopeRemoved reports an explicit project selection with no visible
	// project left. A dynamic selection never returns it.
	ErrScopeRemoved = errors.New("event project selection has no visible project")
)

// ErrPrincipalRevoked is returned by Authority.Reacquire when the persisted
// principal no longer holds authority.
var ErrPrincipalRevoked = ErrRevoked

// ErrAuthorityUnavailable is returned by Authority.Reacquire when the
// decision cannot be made right now, so the caller must neither allow nor end.
var ErrAuthorityUnavailable = ErrUnavailable

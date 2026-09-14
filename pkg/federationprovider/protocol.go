// Package federationprovider lets Kata ask a local helper for access to a hub
// project without sharing hub administration credentials.
//
// Kata saves a token before calling Exchange. The helper reads it with
// DecodeRequest, checks project access, and replies with WriteResponse.
// Approval confirms that saved token; it never replaces it. Pending approval
// and denial are normal responses, while malformed input and helper failures
// return errors. The caller owns storage and retries.
//
// Helpers that forward another service's reply use DecodeResponse to check it
// before forwarding. See docs/development/embedding.md for the wire contract.
package federationprovider

import (
	"encoding/json/v2"
	"errors"
	"io"
	"time"
	"uuid"
)

// MaxDocumentBytes bounds each request and response, including whitespace.
// Documents contain one operation's metadata, never project data.
const MaxDocumentBytes = 16 * 1024

// AttemptTimeout bounds one helper invocation so a stalled provider cannot
// hold a reconciliation attempt indefinitely. Retry belongs to the caller.
const AttemptTimeout = 60 * time.Second

// Intent describes how the local project will use its hub.
type Intent string

// Supported intents distinguish read-only, write, and existing-data adoption.
const (
	IntentReadOnly    Intent = "read_only"
	IntentCollaborate Intent = "collaborate"
	IntentMigrate     Intent = "migrate"
)

// Status is a provider decision, not a process exit code.
type Status string

// Provider decisions describe access or cleanup for the retained request.
const (
	StatusReady            Status = "ready"
	StatusApprovalRequired Status = "approval_required"
	StatusSignInRequired   Status = "sign_in_required"
	StatusDenied           Status = "denied"
	StatusConflict         Status = "conflict"
	StatusUnavailable      Status = "unavailable"
	StatusReleased         Status = "released"
)

var (
	// ErrInvalidRequest indicates malformed input or a helper's exit code 2.
	ErrInvalidRequest = errors.New("invalid federation provider request")
	// ErrInvalidResponse indicates output that cannot authorize the request.
	ErrInvalidResponse = errors.New("invalid federation provider response")
	// ErrProviderFailed indicates a failed executable exchange, not a denial.
	ErrProviderFailed = errors.New("federation credential provider failed")
)

// Request identifies a retained authorization attempt. Release may repeat the
// original target fields, but never CandidateToken. That context identifies the
// saved request; it cannot select a different enrollment to revoke. The caller
// saves CandidateToken before authorize and reuses it unchanged on retries.
type Request struct {
	Version          int       `json:"version"`
	Operation        string    `json:"operation"`
	RequestID        uuid.UUID `json:"request_id"`
	HubURL           string    `json:"hub_url,omitempty"`
	Project          string    `json:"project,omitempty"`
	SpokeInstanceUID string    `json:"spoke_instance_uid,omitempty"`
	LocalProjectUID  string    `json:"local_project_uid,omitempty"`
	Intent           Intent    `json:"intent,omitempty"`
	CandidateToken   string    `json:"candidate_token,omitempty"`
}

// Response is an authorization decision, never a credential. Only ready may
// include the enrollment fields. Message is optional, non-secret display text,
// not a command. A valid denial is a successful exchange, not a process error.
type Response struct {
	Version      int       `json:"version"`
	Operation    string    `json:"operation"`
	RequestID    uuid.UUID `json:"request_id"`
	Status       Status    `json:"status"`
	Message      string    `json:"message,omitempty"`
	HubURL       string    `json:"hub_url,omitempty"`
	ProjectID    int64     `json:"project_id,omitzero"`
	ProjectUID   string    `json:"project_uid,omitempty"`
	EnrollmentID int64     `json:"enrollment_id,omitzero"`
	Actor        string    `json:"actor,omitempty"`
	Capabilities string    `json:"capabilities,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// DecodeRequest reads exactly one bounded request. Errors omit parser details
// because those can contain the candidate token. Providers should exit 2 for
// invalid input, before performing any authorization or release operation.
func DecodeRequest(reader io.Reader) (Request, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return Request{}, ErrInvalidRequest
	}
	return decodeRequest(data)
}

// DecodeResponse reads one bounded result and checks it against the exact
// request. Providers that relay another service's decision use this before
// forwarding it; decoding into Response alone does not enforce the contract.
func DecodeResponse(reader io.Reader, request Request) (Response, error) {
	if !validRequest(request) {
		return Response{}, ErrInvalidRequest
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxDocumentBytes+1))
	if err != nil {
		return Response{}, ErrInvalidResponse
	}
	return decodeResponse(data, request)
}

// WriteResponse validates the entire result before writing one JSON document.
// A provider exits 0 only after this succeeds, including for domain denials.
// On a write failure, exit nonzero: clients must discard partial output.
func WriteResponse(writer io.Writer, request Request, response Response) error {
	if !validRequest(request) {
		return ErrInvalidRequest
	}
	data, err := json.Marshal(response)
	if err != nil {
		return ErrInvalidResponse
	}
	if _, err := decodeResponse(data, request); err != nil {
		return err
	}
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

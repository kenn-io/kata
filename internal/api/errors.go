//nolint:revive // api is the public package namespace used throughout the repo.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// ErrorBody is the inner payload of an error envelope.
type ErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}

// ErrorEnvelope is the stable wire shape for non-2xx responses.
type ErrorEnvelope struct {
	Status int       `json:"status"`
	Error  ErrorBody `json:"error"`
}

// APIError is the Go representation that handlers return; satisfies Huma's
// StatusError interface so the framework serializes the envelope verbatim.
// Plan 1 §4.6 fixes the public name as APIError; renaming would break the
// documented wire contract and CLI parser.
//
//nolint:revive // see comment above re: fixed Plan 1 §4.6 public name.
type APIError struct {
	Status  int
	Code    string
	Message string
	Hint    string
	Data    map[string]any
}

// NewError constructs an APIError. Hint and data are optional.
func NewError(status int, code, message, hint string, data map[string]any) *APIError {
	return &APIError{Status: status, Code: code, Message: message, Hint: hint, Data: data}
}

// Error implements the standard error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// GetStatus implements huma.StatusError so the framework picks the right code.
func (e *APIError) GetStatus() int { return e.Status }

// Envelope returns the JSON body shape used in responses.
func (e *APIError) Envelope() ErrorEnvelope {
	return ErrorEnvelope{
		Status: e.Status,
		Error: ErrorBody{
			Code:    e.Code,
			Message: e.Message,
			Hint:    e.Hint,
			Data:    e.Data,
		},
	}
}

// MarshalJSON serializes the envelope so Huma's default response writer emits
// our wire shape rather than the framework default.
func (e *APIError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.Envelope())
}

// Schema implements huma.SchemaProvider so the generated OpenAPI document
// describes error responses with the true wire shape (ErrorEnvelope) rather
// than APIError's raw Go fields. Because APIError serializes via MarshalJSON,
// Huma's struct reflection would otherwise publish the flat, exported-name
// layout instead of the {status, error:{code,message,...}} envelope clients
// actually receive — a schema that lies about every error response.
func (e *APIError) Schema(r huma.Registry) *huma.Schema {
	return r.Schema(reflect.TypeOf(ErrorEnvelope{}), true, "ErrorEnvelope")
}

// InstallErrorFormatter wires Huma so non-API-typed errors (panics, validation
// failures) also serialize to ErrorEnvelope. Call once at server startup.
// Huma emits 422 for request-body validation failures; we normalize that to
// 400 with code "validation" so the wire contract documented in spec §4.7
// (no 422 in the status table) holds.
//
// Validation errors arrive with a generic message ("validation failed") plus
// a slice of huma.ErrorDetail entries that carry the per-field details.
// Without folding the details into the message, both human ("kata: validation
// failed") and JSON envelope payloads tell the user nothing about what went
// wrong (hammer-test finding #11). Fold up to ~3 details into the surfaced
// message in "field: reason" form so close --reason banana, link 3 banana 4,
// and list --status nonsense surface actionable feedback.
func InstallErrorFormatter() {
	huma.NewError = func(status int, message string, errs ...error) huma.StatusError {
		if status == http.StatusUnprocessableEntity {
			status = http.StatusBadRequest
		}
		return &APIError{
			Status:  status,
			Code:    codeForStatus(status),
			Message: foldDetailsIntoMessage(message, errs),
		}
	}
}

// foldDetailsIntoMessage appends per-error detail to the generic huma
// validation message. Caps at 3 details so a request with many invalid
// fields doesn't produce a multi-paragraph error string; an "and N more"
// suffix replaces the truncated tail.
func foldDetailsIntoMessage(message string, errs []error) string {
	if len(errs) == 0 {
		return message
	}
	const maxDetails = 3
	parts := make([]string, 0, maxDetails)
	for i, e := range errs {
		if i == maxDetails {
			parts = append(parts, fmt.Sprintf("(and %d more)", len(errs)-maxDetails))
			break
		}
		parts = append(parts, formatErrorDetail(e))
	}
	return message + ": " + strings.Join(parts, "; ")
}

// formatErrorDetail extracts a "location: message" pair from an
// ErrorDetailer when possible; falls back to the error's own string.
// huma's validator sets Location for body/path/query field violations
// so the user can tell which field failed.
func formatErrorDetail(e error) string {
	if d, ok := e.(huma.ErrorDetailer); ok {
		det := d.ErrorDetail()
		if det != nil {
			loc := strings.TrimPrefix(det.Location, "body.")
			loc = strings.TrimPrefix(loc, "query.")
			loc = strings.TrimPrefix(loc, "path.")
			if loc != "" {
				return loc + ": " + det.Message
			}
			return det.Message
		}
	}
	return e.Error()
}

func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "validation"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusPreconditionFailed:
		return "confirm_required"
	case http.StatusInternalServerError:
		return "internal"
	default:
		return "error"
	}
}

// WriteEnvelope writes an ErrorEnvelope JSON body with the given status code
// and Content-Type: application/json. Used by HTTP middleware that needs to
// emit the same wire shape as handler-returned APIErrors.
func WriteEnvelope(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(ErrorEnvelope{
		Status: status,
		Error:  ErrorBody{Code: code, Message: message},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// EnsureCancelled is a small helper so handlers can early-return when ctx is
// cancelled without producing a 500 envelope.
func EnsureCancelled(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return NewError(499, "client_closed", err.Error(), "", nil)
	}
	return nil
}

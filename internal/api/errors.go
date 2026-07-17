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

// WrapErrorAdapter keeps Huma's validation status normalization local to one
// API. Huma writes a response status before running response transformers, so
// the adapter context must translate 422 to Kata's documented 400 at the first
// SetStatus call. This avoids mutating Huma's process-global error factories,
// which would otherwise affect unrelated APIs in an embedding host.
func WrapErrorAdapter(adapter huma.Adapter) huma.Adapter {
	return &errorAdapter{Adapter: adapter}
}

type errorAdapter struct {
	huma.Adapter
}

func (a *errorAdapter) Handle(operation *huma.Operation, handler func(huma.Context)) {
	a.Adapter.Handle(operation, func(ctx huma.Context) {
		handler(&errorContext{embeddedContext: ctx})
	})
}

type embeddedContext = huma.Context

type errorContext struct {
	embeddedContext
}

func (c *errorContext) SetStatus(status int) {
	c.embeddedContext.SetStatus(normalizeHumaStatus(status))
}

// TransformHumaError converts framework-generated errors to Kata's stable
// envelope. Explicit APIError values pass through unchanged. Setting the
// content type here also prevents Huma's default ErrorModel from leaving an
// application/problem+json header behind after transformation.
func TransformHumaError(ctx huma.Context, _ string, value any) (any, error) {
	if _, ok := value.(*APIError); ok {
		return value, nil
	}

	if model, ok := value.(*huma.ErrorModel); ok {
		ctx.SetHeader("Content-Type", "application/json")
		return apiErrorFromHuma(model), nil
	}

	if statusError, ok := value.(huma.StatusError); ok {
		status := normalizeHumaStatus(statusError.GetStatus())
		ctx.SetHeader("Content-Type", "application/json")
		return &APIError{
			Status:  status,
			Code:    codeForStatus(status),
			Message: statusError.Error(),
		}, nil
	}

	return value, nil
}

func apiErrorFromHuma(model *huma.ErrorModel) *APIError {
	status := normalizeHumaStatus(model.Status)
	details := make([]error, 0, len(model.Errors))
	for _, detail := range model.Errors {
		if detail != nil {
			details = append(details, detail)
		}
	}
	return &APIError{
		Status:  status,
		Code:    codeForStatus(status),
		Message: foldDetailsIntoMessage(model.Detail, details),
	}
}

func normalizeHumaStatus(status int) int {
	if status == http.StatusUnprocessableEntity {
		return http.StatusBadRequest
	}
	return status
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

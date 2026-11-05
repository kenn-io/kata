package api

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// CronValidationBody documents the raw preflight envelope. Definition remains
// unrestricted so malformed documents can be reported as validation results.
type CronValidationBody struct {
	Name             string `json:"name,omitempty" doc:"Missing or empty names produce request-stage errors."`
	Definition       any    `json:"definition,omitempty" doc:"Missing or invalid definitions produce document-stage errors."`
	UID              string `json:"uid,omitempty"`
	ExpectedEventUID string `json:"expected_event_uid,omitempty"`
	Actor            string `json:"actor,omitempty" doc:"Accepted for create/update file compatibility; ignored."`
}

// CronValidationEnvelope contains validated strings and an unrestricted document.
type CronValidationEnvelope map[string]jsontext.Value

// String reads an optional string already checked by DecodeCronValidationEnvelope.
func (fields CronValidationEnvelope) String(key string) string {
	var value string
	// Absence means empty; present values have already passed string decoding.
	_ = json.Unmarshal(fields[key], &value)
	return value
}

// DecodeCronValidationEnvelope validates envelope types before any typed coercion.
// The definition remains raw, including malformed documents and exact numbers.
// On success the envelope is non-nil and callers may add checked fields.
func DecodeCronValidationEnvelope(raw []byte) (CronValidationEnvelope, error) {
	fields := make(CronValidationEnvelope)
	if jsontext.Value(raw).Kind() != '{' {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("invalid request body: %w", err)
	}
	for key, value := range fields {
		switch key {
		case "definition":
		case "name", "uid", "expected_event_uid", "actor":
			if value.Kind() != '"' {
				return nil, fmt.Errorf("%s must be a string", key)
			}
		default:
			return nil, fmt.Errorf("unknown request member: %s", key)
		}
	}
	return fields, nil
}

// ValidateCronWorkflowRequest keeps envelope and document decoding in the handler.
type ValidateCronWorkflowRequest struct {
	ProjectID int64  `path:"project_id"`
	From      string `query:"from" doc:"RFC3339 preview start; defaults to daemon now."`
	RawBody   []byte `contentType:"application/json"`
}

// ValidateCronJobRequest also selects the bounded number of future instants.
type ValidateCronJobRequest struct {
	ValidateCronWorkflowRequest
	Count int `query:"count" default:"5" minimum:"1" maximum:"100" doc:"Number of next fires, from 1 through 100; defaults to 5."`
}

// CronValidationResult describes a hypothetical write and trigger preview.
type CronValidationResult struct {
	Valid      bool                       `json:"valid"`
	Name       string                     `json:"name"`
	Definition JSONNullableRawObject      `json:"definition"`
	Errors     []db.CronValidationError   `json:"errors"`
	Warnings   []db.CronValidationWarning `json:"warnings"`
	Schedule   *cron.SchedulePreview      `json:"schedule,omitempty"`
}

// CronValidationResponse returns the result without creating any cron state.
type CronValidationResponse struct{ Body CronValidationResult }

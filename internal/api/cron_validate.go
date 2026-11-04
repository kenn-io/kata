package api

import (
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

// CronValidationBody documents the raw preflight envelope. Definition remains
// unrestricted so malformed documents can be reported as validation results.
type CronValidationBody struct {
	Name             string `json:"name"`
	Definition       any    `json:"definition"`
	UID              string `json:"uid,omitempty"`
	ExpectedEventUID string `json:"expected_event_uid,omitempty"`
	Actor            string `json:"actor,omitempty" doc:"Accepted for create/update file compatibility; ignored."`
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
	Count string `query:"count" doc:"Number of next fires, from 1 through 100; defaults to 5."`
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

// CronValidationFrom keeps opaque document numbers intact on the API wire.
func CronValidationFrom(value db.CronValidation) CronValidationResult {
	return CronValidationResult{Valid: value.Valid, Name: value.Name, Definition: JSONNullableRawObject(value.Definition), Errors: value.Errors, Warnings: value.Warnings, Schedule: value.Schedule}
}

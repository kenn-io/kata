package api

import "go.kenn.io/kata/internal/diagnostics"

// DoctorResponse exposes sanitized process-local hook diagnostics to operators.
type DoctorResponse struct {
	Body struct {
		Hooks diagnostics.Hooks `json:"hooks"`
	}
}

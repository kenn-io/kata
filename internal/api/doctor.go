package api

import "go.kenn.io/kata/internal/diagnostics"

// DoctorResponse exposes sanitized process-local hook diagnostics to operators.
type DoctorResponse struct {
	Body struct {
		Hooks      diagnostics.Hooks `json:"hooks"`
		AgentsView DoctorAgentsView  `json:"agentsview"`
	}
}

// DoctorAgentsView reports the serving daemon's on-demand usage probe.
type DoctorAgentsView struct {
	State string `json:"state" enum:"off,ok,unreachable,unauthorized,not_agentsview"`
}

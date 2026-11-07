package api

import "go.kenn.io/kata/internal/db"

// CronRequest is advisory work for optional runtimes, with current read state.
type CronRequest db.CronRequest

// CronRequestBody binds a caller-retained fresh UID to its retry identity.
type CronRequestBody struct {
	UID        string `json:"uid" minLength:"26" maxLength:"26"`
	Actor      string `json:"actor,omitempty"`
	TTLSeconds *int   `json:"ttl_seconds,omitempty" minimum:"60" maximum:"86400"`
}

// CronJobRequestRun addresses an adapter execute job.
type CronJobRequestRun struct {
	CronDefinitionRequest
	Body CronRequestBody
}

// CronRunRequestCancel addresses a currently running observation.
type CronRunRequestCancel struct {
	CronRunRequest
	Body CronRequestBody
}

// CronRequestResponse returns the original pinned request on an exact retry.
type CronRequestResponse struct {
	Body struct {
		Request  CronRequest `json:"request"`
		Replayed bool        `json:"replayed"`
		Event    *db.Event   `json:"event,omitempty"`
	}
}

// CronRequestListRequest reads retained requests in one project.
type CronRequestListRequest struct {
	CronProjectRequest
	State string `query:"state" enum:"pending,answered,expired"`
}

// CronRequestListResponse contains locally known retained requests.
type CronRequestListResponse struct {
	Body struct {
		Requests []CronRequest `json:"requests"`
	}
}

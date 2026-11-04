package api

import (
	"time"

	"go.kenn.io/kata/internal/db"
)

// PruneCronRunsBody selects one batch; previews use the same authority checks.
type PruneCronRunsBody struct {
	Actor       string    `json:"actor,omitempty"`
	Before      time.Time `json:"before"`
	Keep        int       `json:"keep,omitempty" default:"10" minimum:"1" maximum:"1000"`
	Limit       int       `json:"limit,omitempty" default:"5000" minimum:"1" maximum:"20000"`
	JobUID      string    `json:"job_uid,omitempty"`
	WorkflowUID string    `json:"workflow_uid,omitempty"`
	DryRun      bool      `json:"dry_run"`
}

// PruneCronRunsRequest confirms irreversible removal at the owning hub.
type PruneCronRunsRequest struct {
	CronProjectRequest
	Confirm string `header:"X-Kata-Confirm"`
	Body    PruneCronRunsBody
}

// PruneCronRunsResponse reports the selected batch and its committed reset.
type PruneCronRunsResponse struct {
	Body db.CronRunPruneResult
}

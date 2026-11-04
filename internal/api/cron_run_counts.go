package api

import "go.kenn.io/kata/internal/db"

// CronRunCountsRequest selects aggregate identity, not workflow membership.
type CronRunCountsRequest struct {
	CronProjectRequest
	JobUID      string `query:"job_uid"`
	WorkflowUID string `query:"workflow_uid"`
}

// CronRunCountsResponse separates retained evidence from recorded pruning.
type CronRunCountsResponse struct{ Body db.CronRunCountsResult }

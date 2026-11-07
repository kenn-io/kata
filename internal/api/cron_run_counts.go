package api

import (
	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
)

// CronRunCountsRequest selects aggregate identity, not workflow membership.
type CronRunCountsRequest struct {
	CronProjectRequest
	JobUID      string `query:"job_uid"`
	WorkflowUID string `query:"workflow_uid"`
}

// Resolve rejects a present empty filter, which Huma otherwise binds as
// omitted and would widen the counts to every group in the project.
func (*CronRunCountsRequest) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	q := u.Query()
	for _, name := range []string{"job_uid", "workflow_uid"} {
		if q.Has(name) && q.Get(name) == "" {
			return []error{NewError(400, "validation", name+" must be a 26-character ULID", "", nil)}
		}
	}
	return nil
}

// CronRunCountsResponse separates retained evidence from recorded pruning.
type CronRunCountsResponse struct{ Body db.CronRunCountsResult }

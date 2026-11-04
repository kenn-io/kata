package cronread

import (
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ProjectQuery serializes normalized predicates without sending omitted values.
func ProjectQuery(in db.CronRunList) *generated.ListCronRunsQuery {
	out := &generated.ListCronRunsQuery{Limit: new(int64(in.Limit)), BeforeUID: optional(in.BeforeUID), JobUID: optional(in.JobUID), Status: in.Statuses, IssueUID: optional(in.IssueUID), ExecutorLabel: optional(in.ExecutorLabel), Teammate: optional(in.Teammate)}
	if !in.Since.IsZero() {
		out.Since = new(in.Since.UTC().Format(time.RFC3339Nano))
	}
	return out
}

// FleetQuery adds the explicit project selection to the same predicates.
func FleetQuery(in db.CronRunList, ids []int64) *generated.ListAllCronRunsQuery {
	q := ProjectQuery(in)
	return &generated.ListAllCronRunsQuery{ProjectIds: ids, Limit: q.Limit, BeforeUID: q.BeforeUID, JobUID: q.JobUID, Status: q.Status, IssueUID: q.IssueUID, ExecutorLabel: q.ExecutorLabel, Teammate: q.Teammate, Since: q.Since}
}

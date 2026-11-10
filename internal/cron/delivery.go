package cron

import "time"

// RecurringBehindAfter allows four default sweeps before reporting a backlog.
const RecurringBehindAfter = 2 * time.Minute

// RecurringDescription is a dry run, without changing the durable checkpoint.
type RecurringDescription struct {
	NextDueAt time.Time
	Batch     RecurringBatch
}

// DescribeRecurring evaluates even paused definitions, as if delivery resumed.
func DescribeRecurring(j JobDefinition, definitionEventUID string, p RecurringProgress, now time.Time) (RecurringDescription, error) {
	j.Enabled = true
	batch, err := PlanRecurring(j, definitionEventUID, p, now)
	if err != nil {
		return RecurringDescription{}, err
	}
	next, err := recurringNext(j.Trigger, p.Anchor)
	if err != nil {
		return RecurringDescription{}, err
	}
	due := next(p.Through)
	if due.IsZero() || !due.After(p.Through) || due.Year() > 9999 {
		return RecurringDescription{}, invalid("schedule has no supported next occurrence")
	}
	return RecurringDescription{NextDueAt: due.UTC(), Batch: batch}, nil
}

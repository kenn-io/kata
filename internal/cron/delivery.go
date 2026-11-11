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
	occurrences, err := RecurringOccurrences(j, p, p.Through, 1)
	if err != nil {
		return RecurringDescription{}, err
	}
	if len(occurrences) == 0 {
		return RecurringDescription{}, invalid("schedule has no supported next occurrence")
	}
	return RecurringDescription{NextDueAt: occurrences[0], Batch: batch}, nil
}

package cron

import (
	"time"

	cronparser "github.com/netresearch/go-cron"
)

// RecurringSkip counts finalized discarded occurrences in (After, Before).
// Capped means Count is a lower bound rather than an exact total.
type RecurringSkip struct {
	Count  int       `json:"count"`
	Capped bool      `json:"capped"`
	After  time.Time `json:"after,omitzero"`
	Before time.Time `json:"before,omitzero"`
}

func countRecurringSkips(trigger Trigger, next func(time.Time) time.Time, after, before time.Time) RecurringSkip {
	first := next(after)
	if first.IsZero() || !first.After(after) || !first.Before(before) {
		return RecurringSkip{}
	}
	out := RecurringSkip{After: after, Before: before}
	if trigger.Kind == "interval" {
		elapsed := before.Sub(first)
		if elapsed < time.Duration(1<<63-1) {
			out.Count = int((elapsed-1)/(time.Duration(trigger.IntervalSeconds)*time.Second)) + 1
			return out
		}
	}
	out.Count = cronparser.CountWithLimit(recurringStep(next), after, before, 1001)
	out.Capped = out.Count > 1000
	if out.Capped {
		out.Count = 1000
	}
	return out
}

type recurringStep func(time.Time) time.Time

func (step recurringStep) Next(cursor time.Time) time.Time { return step(cursor) }

package cron

import "time"

// recurringNext returns exactly the stepping function used by PlanRecurring.
func recurringNext(t Trigger, anchor time.Time) (func(time.Time) time.Time, error) {
	if t.Kind != "interval" {
		return recurringSchedule(t)
	}
	step := time.Duration(t.IntervalSeconds) * time.Second
	return func(cursor time.Time) time.Time {
		elapsed := cursor.Sub(anchor)
		// Saturating time.Sub must never masquerade as a different occurrence.
		if elapsed == time.Duration(1<<63-1) {
			return time.Time{}
		}
		remaining := step - elapsed%step
		return cursor.Add(remaining).UTC()
	}, nil
}

// RecurringLocation is the zone the native stepping actually evaluates in.
// Robfig's Local sentinel uses the cursor's zone; native cursors are UTC.
func RecurringLocation(t Trigger) (*time.Location, error) {
	if t.Timezone == "" || t.Timezone == "Local" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(t.Timezone)
	if err != nil {
		return nil, invalid("invalid timezone")
	}
	return loc, nil
}

// RecurringOccurrences returns up to n occurrences strictly after from and
// the binding anchor, using the same stepping as PlanRecurring. Results are UTC.
// Callers choose their own definition validation policy.
func RecurringOccurrences(t Trigger, anchor, from time.Time, n int) ([]time.Time, error) {
	if n < 1 {
		return nil, invalid("occurrence count must be positive")
	}
	switch t.Kind {
	case "cron":
	case "interval":
		if t.IntervalSeconds < 1 {
			return nil, invalid("interval must be positive")
		}
		if t.IntervalSeconds > maxDurationSeconds {
			return nil, invalid("interval exceeds supported duration")
		}
		if anchor.IsZero() {
			return nil, invalid("interval preview requires an anchor")
		}
	default:
		return nil, invalid("preview supports cron, interval and once triggers; this job uses " + t.Kind)
	}
	if _, err := RecurringLocation(t); err != nil {
		return nil, err
	}
	anchor, from = anchor.UTC(), from.UTC()
	next, err := recurringNext(t, anchor)
	if err != nil {
		return nil, err
	}
	cursor := from
	if !anchor.IsZero() && anchor.After(cursor) {
		cursor = anchor
	}
	out := make([]time.Time, 0)
	for range n {
		due := next(cursor)
		if due.IsZero() || !due.After(cursor) || due.Year() > 9999 {
			if len(out) == 0 {
				return nil, invalid("schedule has no supported next occurrence")
			}
			break
		}
		cursor = due.UTC()
		out = append(out, cursor)
	}
	return out, nil
}

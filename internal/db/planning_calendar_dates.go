package db

import "go.kenn.io/kata/internal/metadata"

// planningCalendarDates holds both planning fields' calendar dates. Each field
// resolves independently, so one unreadable value leaves the other usable.
type planningCalendarDates struct {
	scheduledOn, deadlineOn       string
	hasScheduledOn, hasDeadlineOn bool
	scheduledErr, deadlineErr     error
}

func (d planningCalendarDates) err() error {
	if d.scheduledErr != nil {
		return d.scheduledErr
	}
	return d.deadlineErr
}

// resolvePlanningCalendarDates is the one rule that turns scheduled_on and
// deadline_on metadata into YYYY-MM-DD calendar dates. The browser snapshot
// and the list and show rows both call it, so they cannot disagree. A non-nil
// display is the viewer's zone; nil reads each value in its own schedule zone.
// Each field takes its own default timezone for date-times that carry no issue
// timezone; scheduled dates can include the legacy recurrence fallback.
func resolvePlanningCalendarDates(
	raw string, display *string, scheduledDefaultTimezone, deadlineDefaultTimezone string,
) planningCalendarDates {
	var out planningCalendarDates
	out.scheduledOn, out.hasScheduledOn, out.scheduledErr = metadata.CalendarDate(
		raw, "scheduled_on", display, scheduledDefaultTimezone,
	)
	out.deadlineOn, out.hasDeadlineOn, out.deadlineErr = metadata.CalendarDate(
		raw, "deadline_on", display, deadlineDefaultTimezone,
	)
	return out
}

// RowPlanningDates returns the scheduled_on and deadline_on calendar dates
// carried by list and show rows. A value is read in its issue's timezone, then
// the field's default timezone, then UTC, so a request needs no zone. A field
// is empty when its key is absent or its stored value cannot be resolved, so
// one unreadable value never fails a whole response. scheduledDefaultTimezone
// can include the legacy recurrence fallback; deadlines use the daemon
// timezone.
func RowPlanningDates(raw, scheduledDefaultTimezone, deadlineDefaultTimezone string) (scheduledOn, deadlineOn string) {
	dates := resolvePlanningCalendarDates(raw, nil, scheduledDefaultTimezone, deadlineDefaultTimezone)
	if dates.scheduledErr == nil {
		scheduledOn = dates.scheduledOn
	}
	if dates.deadlineErr == nil {
		deadlineOn = dates.deadlineOn
	}
	return scheduledOn, deadlineOn
}

package db

import (
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// planningCalendarDates holds both planning fields' calendar dates. Each field
// resolves independently, so one unreadable value leaves the other usable.
type planningCalendarDates struct {
	scheduledOn, deadlineOn           string
	scheduledToday, deadlineToday     string
	scheduledDisplay, deadlineDisplay string
	hasScheduledOn, hasDeadlineOn     bool
	scheduledErr, deadlineErr         error
}

func (d planningCalendarDates) err() error {
	if d.scheduledErr != nil {
		return d.scheduledErr
	}
	return d.deadlineErr
}

// unsupported returns the error of a field saved by a newer build (tzdata rules
// or members this build cannot read), which a row must surface, not omit.
func (d planningCalendarDates) unsupported() error {
	for _, err := range []error{d.scheduledErr, d.deadlineErr} {
		if metadata.IsUnsupportedSchedule(err) {
			return err
		}
	}
	return nil
}

// resolvePlanningCalendarDates is the one rule that turns scheduled_on and
// deadline_on metadata into YYYY-MM-DD calendar dates. The browser snapshot
// and the list and show rows both call it, so they cannot disagree. A non-nil
// display is the viewer's zone; nil reads each value in its own schedule zone.
// Each field takes its own default timezone for date-times that carry no issue
// timezone; scheduled dates can include the legacy recurrence fallback.
func resolvePlanningCalendarDates(
	raw string, display *string, scheduledDefaultTimezone, deadlineDefaultTimezone string, now time.Time, boundary *time.Time,
) planningCalendarDates {
	var out planningCalendarDates
	out.scheduledOn, out.scheduledToday, out.hasScheduledOn, out.scheduledErr = metadata.CalendarDateAndToday(
		raw, "scheduled_on", display, scheduledDefaultTimezone, now, boundary, &out.scheduledDisplay,
	)
	out.deadlineOn, out.deadlineToday, out.hasDeadlineOn, out.deadlineErr = metadata.CalendarDateAndToday(
		raw, "deadline_on", display, deadlineDefaultTimezone, now, boundary, &out.deadlineDisplay,
	)
	return out
}

// RowPlanningDates returns the scheduled_on and deadline_on calendar dates
// carried by list and show rows. A value is read in its issue's timezone, then
// the field's default timezone, then UTC, so a request needs no zone. A field
// is empty when its key is absent or its stored value cannot be resolved, so
// one malformed value never fails a whole response; only a saved schedule with
// an unsupported tzdata version or member returns an error. scheduledDefaultTimezone can
// include the legacy recurrence fallback; deadlines use the daemon timezone.
func RowPlanningDates(raw, scheduledDefaultTimezone, deadlineDefaultTimezone string) (scheduledOn, deadlineOn string, err error) {
	dates := resolvePlanningCalendarDates(raw, nil, scheduledDefaultTimezone, deadlineDefaultTimezone, time.Time{}, nil)
	return dates.scheduledOn, dates.deadlineOn, dates.unsupported()
}

// RowPlanningProjection resolves each field and the current day in its zone.
func RowPlanningProjection(raw, scheduledDefaultTimezone, deadlineDefaultTimezone string, now time.Time) (scheduledOn, deadlineOn, scheduledToday, deadlineToday, scheduledDisplay, deadlineDisplay string, err error) {
	dates := resolvePlanningCalendarDates(raw, nil, scheduledDefaultTimezone, deadlineDefaultTimezone, now, nil)
	return dates.scheduledOn, dates.deadlineOn, dates.scheduledToday, dates.deadlineToday, dates.scheduledDisplay, dates.deadlineDisplay, dates.unsupported()
}

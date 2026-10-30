package db

import "go.kenn.io/kata/internal/metadata"

// planningDisplay names the zone a planning date is read in: an explicit
// viewer zone (the browser's, UTC when empty), or the zone the value is
// scheduled in (the issue timezone, then the field's default, then UTC).
type planningDisplay struct {
	zone           string
	inScheduleZone bool
}

type planningCalendarDates struct {
	scheduledOn, deadlineOn       string
	hasScheduledOn, hasDeadlineOn bool
}

// resolvePlanningCalendarDates is the one rule that turns scheduled_on and
// deadline_on metadata into YYYY-MM-DD calendar dates. The browser snapshot
// and the list and show rows all call it, so they cannot disagree. Each
// field takes its own default timezone for date-times that carry no issue
// timezone; scheduled dates can include the legacy recurrence fallback.
func resolvePlanningCalendarDates(
	raw string,
	scheduledDisplay planningDisplay, scheduledDefaultTimezone string,
	deadlineDisplay planningDisplay, deadlineDefaultTimezone string,
) (planningCalendarDates, error) {
	var out planningCalendarDates
	var err error
	out.scheduledOn, out.hasScheduledOn, err = planningField(
		raw, "scheduled_on", scheduledDisplay, scheduledDefaultTimezone,
	)
	if err != nil {
		return planningCalendarDates{}, err
	}
	out.deadlineOn, out.hasDeadlineOn, err = planningField(
		raw, "deadline_on", deadlineDisplay, deadlineDefaultTimezone,
	)
	if err != nil {
		return planningCalendarDates{}, err
	}
	return out, nil
}

func planningField(raw, field string, display planningDisplay, defaultTimezone string) (string, bool, error) {
	switch {
	case field == "scheduled_on" && display.inScheduleZone:
		return metadata.ScheduledOnScheduleZoneDate(raw, defaultTimezone)
	case field == "scheduled_on":
		return metadata.ScheduledOnCalendarDate(raw, display.zone, defaultTimezone)
	case display.inScheduleZone:
		return metadata.DeadlineOnScheduleZoneDate(raw, defaultTimezone)
	default:
		return metadata.DeadlineOnCalendarDate(raw, display.zone, defaultTimezone)
	}
}

// RowPlanningDates returns the scheduled_on and deadline_on calendar dates
// carried by list and show rows, each empty when its key is absent. A value
// is read in its issue's timezone, then the field's default timezone, then
// UTC, so a request needs no zone. scheduledDefaultTimezone can include the
// legacy recurrence fallback; deadlines use the daemon timezone.
func RowPlanningDates(
	raw, scheduledDefaultTimezone, deadlineDefaultTimezone string,
) (scheduledOn, deadlineOn string, err error) {
	dates, err := resolvePlanningCalendarDates(
		raw,
		planningDisplay{inScheduleZone: true}, scheduledDefaultTimezone,
		planningDisplay{inScheduleZone: true}, deadlineDefaultTimezone,
	)
	if err != nil {
		return "", "", err
	}
	return dates.scheduledOn, dates.deadlineOn, nil
}

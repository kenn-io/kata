package cron

import (
	"time"

	"github.com/robfig/cron/v3"
)

// Validate checks the selected trigger and its kind-specific fields.
func (t Trigger) Validate() error {
	if t.Timezone != "" {
		if _, err := time.LoadLocation(t.Timezone); err != nil {
			return invalid("invalid timezone")
		}
	}
	if t.LeadSeconds < 0 {
		return invalid("negative deadline lead")
	}
	if t.LeadSeconds > maxDurationSeconds {
		return invalid("deadline lead exceeds supported duration")
	}
	switch t.Kind {
	case "manual":
	case "cron":
		schedule, err := ParseCron(t.Cron)
		if err != nil {
			return invalid("invalid cron expression")
		}
		if err := requireCalendarDate(schedule); err != nil {
			return err
		}
	case "interval":
		if t.IntervalSeconds <= 0 {
			return invalid("interval must be positive")
		}
		if t.IntervalSeconds > maxDurationSeconds {
			return invalid("interval exceeds supported duration")
		}
	case "once":
		if _, err := time.Parse(time.RFC3339, t.At); err != nil {
			return invalid("once requires an RFC3339 instant")
		}
	case "issue-scheduled", "issue-deadline":
		if !validUID(t.IssueUID) {
			return invalid("issue date requires an issue UID")
		}
	default:
		return invalid("unknown trigger")
	}
	if t.Kind != "cron" && t.Cron != "" || t.Kind != "interval" && t.IntervalSeconds != 0 || t.Kind != "once" && t.At != "" || t.Kind != "issue-scheduled" && t.Kind != "issue-deadline" && t.IssueUID != "" || t.Kind != "issue-deadline" && t.LeadSeconds != 0 {
		return invalid("trigger carries fields for another kind")
	}
	return nil
}

// calendarReference starts a fixed leap year so robfig's search window sees
// every calendar date without depending on the wall clock.
var calendarReference = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

func requireCalendarDate(schedule cron.Schedule) error {
	spec, ok := schedule.(*cron.SpecSchedule)
	if !ok {
		return nil // @every is a constant delay and always has a next run.
	}
	// Check date existence independently of historical DST gaps, without
	// changing the original schedule's delivery timezone.
	utc := *spec
	utc.Location = time.UTC
	if utc.Next(calendarReference).IsZero() {
		return invalid("cron expression never fires: its day of month does not occur in its months")
	}
	return nil
}

// ParseCron preserves the shared five-field and descriptor grammar. Callers
// evaluating schedules must use this parser rather than another cron dialect.
func ParseCron(expression string) (cron.Schedule, error) {
	return cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor).Parse(expression)
}

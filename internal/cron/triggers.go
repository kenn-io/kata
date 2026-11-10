package cron

import (
	"fmt"
	"time"

	"github.com/netresearch/go-cron"
)

// Validate checks trigger syntax and kind-specific fields.
func (t Trigger) Validate() error {
	_, err := t.validate()
	return err
}

// validate returns the parsed cron schedule for complete job validation to reuse.
func (t Trigger) validate() (cron.Schedule, error) {
	var schedule cron.Schedule
	if t.Timezone != "" {
		if _, err := time.LoadLocation(t.Timezone); err != nil {
			return nil, invalid("invalid timezone")
		}
	}
	if t.LeadSeconds < 0 {
		return nil, invalid("negative deadline lead")
	}
	if t.LeadSeconds > maxDurationSeconds {
		return nil, invalid("deadline lead exceeds supported duration")
	}
	switch t.Kind {
	case "manual":
	case "cron":
		var err error
		schedule, err = ParseCron(t.Cron)
		if err != nil {
			return nil, invalid("invalid cron expression")
		}
	case "interval":
		if t.IntervalSeconds <= 0 {
			return nil, invalid("interval must be positive")
		}
		if t.IntervalSeconds > maxDurationSeconds {
			return nil, invalid("interval exceeds supported duration")
		}
	case "once":
		if _, err := time.Parse(time.RFC3339, t.At); err != nil {
			return nil, invalid("once requires an RFC3339 instant")
		}
	case "issue-scheduled", "issue-deadline":
		if !validUID(t.IssueUID) {
			return nil, invalid("issue date requires an issue UID")
		}
	default:
		return nil, invalid("unknown trigger")
	}
	if t.Kind != "cron" && t.Cron != "" || t.Kind != "interval" && t.IntervalSeconds != 0 || t.Kind != "once" && t.At != "" || t.Kind != "issue-scheduled" && t.Kind != "issue-deadline" && t.IssueUID != "" || t.Kind != "issue-deadline" && t.LeadSeconds != 0 {
		return nil, invalid("trigger carries fields for another kind")
	}
	return schedule, nil
}

// calendarReference starts a fixed leap year so the schedule's search window
// sees every calendar date without depending on the wall clock.
var calendarReference = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

func requireOccurrence(schedule cron.Schedule) error {
	if schedule == nil {
		return nil
	}
	spec, calendar := schedule.(*cron.SpecSchedule)
	if calendar {
		// Check date existence independently of historical DST gaps, without
		// changing the original schedule's delivery timezone.
		utc := *spec
		utc.Location = time.UTC
		schedule = &utc
	}
	if !schedule.Next(calendarReference).IsZero() {
		return nil
	}
	if calendar {
		return fmt.Errorf("%w: its day of month does not occur in its months", ErrNeverFires)
	}
	return ErrNeverFires
}

// cronParser reads the shared five-field and descriptor grammar. DowOrDom keeps
// crontab's rule that restricting both day fields matches either one.
var cronParser = cron.MustNewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor | cron.DowOrDom)

// ParseCron preserves the shared five-field and descriptor grammar. Callers
// evaluating schedules must use this parser rather than another cron dialect.
// The parser rejects a TZ= or CRON_TZ= directive without a following schedule.
func ParseCron(expression string) (cron.Schedule, error) {
	return cronParser.Parse(expression)
}

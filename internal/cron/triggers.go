package cron

import (
	"time"

	"github.com/netresearch/go-cron"
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
		if _, err := ParseCron(t.Cron); err != nil {
			return invalid("invalid cron expression")
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

// cronParser reads the shared five-field and descriptor grammar. DowOrDom keeps
// crontab's rule that restricting both day fields matches either one.
var cronParser = cron.MustNewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor | cron.DowOrDom)

// ParseCron preserves the shared five-field and descriptor grammar. Callers
// evaluating schedules must use this parser rather than another cron dialect.
// The parser rejects a TZ= or CRON_TZ= directive without a following schedule.
func ParseCron(expression string) (cron.Schedule, error) {
	return cronParser.Parse(expression)
}

package cron

import (
	"strings"
	"time"

	cronparser "github.com/netresearch/go-cron"
)

// AdapterSchedule evaluates profile-v1 clock triggers strictly after the cursor.
// Intervals and @every start one effective step after replicated creation.
// Validate here as well: callers can supply a trigger without parsing a job.
func AdapterSchedule(trigger Trigger, createdAt time.Time) (func(time.Time) time.Time, error) {
	if err := trigger.Validate(); err != nil {
		return nil, err
	}
	if createdAt.IsZero() || createdAt.Year() > 9999 {
		return nil, invalid("adapter creation instant required")
	}
	anchor := createdAt.UTC()
	var next func(time.Time) time.Time
	switch trigger.Kind {
	case "interval":
		next = anchoredInterval(anchor, time.Duration(trigger.IntervalSeconds)*time.Second)
	case "once":
		at, _ := time.Parse(time.RFC3339, trigger.At)
		next = func(cursor time.Time) time.Time {
			if at.After(anchor) && at.After(cursor) {
				return at
			}
			return time.Time{}
		}
	case "cron":
		schedule, err := ParseCron(adapterCronExpression(trigger, "UTC"))
		if err != nil {
			return nil, err
		}
		if delay, ok := schedule.(cronparser.ConstantDelaySchedule); ok {
			next = anchoredInterval(anchor, delay.Delay)
		} else {
			next = func(cursor time.Time) time.Time {
				if cursor.Before(anchor) {
					cursor = anchor
				}
				return schedule.Next(cursor)
			}
		}
	default:
		return nil, invalid("trigger has no clock-based adapter schedule")
	}
	return func(cursor time.Time) time.Time {
		at := next(cursor)
		if at.Year() > 9999 || !at.After(cursor) {
			return time.Time{}
		}
		return at.UTC()
	}, nil
}

// adapterCronExpression preserves an embedded directive ahead of caller defaults.
func adapterCronExpression(trigger Trigger, fallback string) string {
	expression := strings.TrimSpace(trigger.Cron)
	_, _, has, _ := splitCronDirective(expression)
	if !has {
		zone := trigger.Timezone
		if zone == "" {
			zone = fallback
		}
		if zone != "" {
			expression = "CRON_TZ=" + zone + " " + expression
		}
	}
	return expression
}

func anchoredInterval(anchor time.Time, step time.Duration) func(time.Time) time.Time {
	return func(cursor time.Time) time.Time {
		if cursor.Before(anchor) {
			return anchor.Add(step)
		}
		// Sub saturates beyond 292 years. Unix seconds preserve distant RFC3339
		// anchors, and the nanosecond remainder preserves fractional creation times.
		seconds := int64(step / time.Second)
		remainder := (cursor.Unix() - anchor.Unix()) % seconds
		nanos := int64(cursor.Nanosecond() - anchor.Nanosecond())
		if nanos < 0 {
			remainder = (remainder - 1 + seconds) % seconds
			nanos += int64(time.Second)
		}
		return cursor.Add(time.Duration(seconds-remainder)*time.Second - time.Duration(nanos))
	}
}

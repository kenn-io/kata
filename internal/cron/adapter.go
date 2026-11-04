package cron

import (
	"strings"
	"time"

	cronparser "github.com/netresearch/go-cron"
)

// AdapterSchedule returns profile-v1 instants strictly after a cursor. Unlike
// native delivery, an adapter's interval anchor is the replicated job creation.
// A zero next instant means there is no supported occurrence.
func AdapterSchedule(t Trigger, createdAt time.Time) (func(time.Time) time.Time, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if createdAt.IsZero() || createdAt.Year() > 9999 {
		return nil, invalid("adapter creation instant required")
	}
	anchor := createdAt.UTC()
	var next func(time.Time) time.Time
	switch t.Kind {
	case "interval":
		next = anchoredInterval(anchor, time.Duration(t.IntervalSeconds)*time.Second)
	case "once":
		instant, _ := time.Parse(time.RFC3339, t.At)
		next = func(cursor time.Time) time.Time {
			if instant.After(anchor) && instant.After(cursor) {
				return instant.UTC()
			}
			return time.Time{}
		}
	case "cron":
		expression := strings.TrimSpace(t.Cron)
		_, _, has, _ := splitCronDirective(expression)
		if !has {
			zone := t.Timezone
			if zone == "" {
				zone = "UTC"
			}
			expression = "CRON_TZ=" + zone + " " + expression
		}
		schedule, err := ParseCron(expression)
		if err != nil {
			return nil, err
		}
		if interval, ok := schedule.(cronparser.ConstantDelaySchedule); ok {
			next = anchoredInterval(anchor, interval.Delay)
		} else {
			next = func(cursor time.Time) time.Time { return schedule.Next(laterOf(cursor, anchor)).UTC() }
		}
	default:
		return nil, invalid("trigger has no clock-based adapter schedule")
	}
	return func(cursor time.Time) time.Time {
		due := next(cursor)
		if due.Year() > 9999 || !due.After(cursor) {
			return time.Time{}
		}
		return due.UTC()
	}, nil
}

func anchoredInterval(anchor time.Time, step time.Duration) func(time.Time) time.Time {
	return func(cursor time.Time) time.Time {
		if cursor.Before(anchor) {
			return anchor.Add(step).UTC()
		}
		// Unix seconds fit across the RFC3339 range, unlike time.Sub's
		// duration, which saturates for otherwise valid distant anchors.
		stepSeconds := int64(step / time.Second)
		remainder := (cursor.Unix() - anchor.Unix()) % stepSeconds
		nanos := int64(cursor.Nanosecond() - anchor.Nanosecond())
		if nanos < 0 {
			remainder = (remainder - 1 + stepSeconds) % stepSeconds
			nanos += int64(time.Second)
		}
		return cursor.Add(time.Duration(stepSeconds-remainder)*time.Second - time.Duration(nanos)).UTC()
	}
}

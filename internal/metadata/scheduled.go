package metadata

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type scheduledOnKind int

const (
	scheduledOnDate scheduledOnKind = iota
	scheduledOnLocalTime
	scheduledOnInstant
)

const (
	localMinuteLayout = "2006-01-02T15:04"
	localSecondLayout = "2006-01-02T15:04:05"
)

type scheduleMetadata struct {
	ScheduledOn *string `json:"scheduled_on"`
	Timezone    string  `json:"timezone"`
}

// ScheduledOnDue reports whether metadata's scheduled_on gate has opened.
// A date or local date-time uses the issue timezone, then the daemon timezone,
// then UTC. A UTC value ending in Z is an instant and ignores those zones.
func ScheduledOnDue(raw string, now time.Time, defaultTimezone string) (bool, error) {
	if len(raw) == 0 {
		return true, nil
	}
	values, err := decodeScheduleMetadata(raw)
	if err != nil {
		return false, err
	}
	if values.ScheduledOn == nil {
		return true, nil
	}

	scheduledOn := *values.ScheduledOn
	kind, layout, instant, err := classifyScheduledOn(scheduledOn)
	if err != nil {
		return false, err
	}
	if kind == scheduledOnDate || kind == scheduledOnLocalTime {
		location, err := loadScheduleLocation(values.Timezone, defaultTimezone)
		if err != nil {
			return false, err
		}
		if kind == scheduledOnDate {
			return scheduledOn <= now.In(location).Format(layout), nil
		}
		resolved, err := resolveLocalSchedule(scheduledOn, layout, location)
		if err != nil {
			return false, err
		}
		return !now.Before(resolved), nil
	}
	return !now.Before(instant), nil
}

// ScheduledOnCalendarDate returns the display calendar date for scheduled_on.
// Date-only values keep their written civil date. Local date-times are first
// resolved in the issue or daemon timezone, while UTC values are already
// instants; both timed forms are then projected into displayTimezone.
func ScheduledOnCalendarDate(
	raw string,
	displayTimezone string,
	defaultTimezone string,
) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	values, err := decodeScheduleMetadata(raw)
	if err != nil {
		return "", false, err
	}
	if values.ScheduledOn == nil {
		return "", false, nil
	}

	scheduledOn := *values.ScheduledOn
	kind, layout, instant, err := classifyScheduledOn(scheduledOn)
	if err != nil {
		return "", false, err
	}
	if kind == scheduledOnDate {
		return scheduledOn, true, nil
	}
	if kind == scheduledOnLocalTime {
		location, err := loadScheduleLocation(values.Timezone, defaultTimezone)
		if err != nil {
			return "", false, err
		}
		instant, err = resolveLocalSchedule(scheduledOn, layout, location)
		if err != nil {
			return "", false, err
		}
	}
	if displayTimezone == "" {
		displayTimezone = "UTC"
	}
	displayLocation, err := time.LoadLocation(displayTimezone)
	if err != nil {
		return "", false, fmt.Errorf("load display timezone %q: %w", displayTimezone, err)
	}
	return instant.In(displayLocation).Format(time.DateOnly), true, nil
}

func decodeScheduleMetadata(raw string) (scheduleMetadata, error) {
	var values scheduleMetadata
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return scheduleMetadata{}, fmt.Errorf("decode schedule metadata: %w", err)
	}
	return values, nil
}

func loadScheduleLocation(issueTimezone, defaultTimezone string) (*time.Location, error) {
	zone := issueTimezone
	if zone == "" {
		zone = defaultTimezone
	}
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("load schedule timezone %q: %w", zone, err)
	}
	return location, nil
}

// resolveLocalSchedule maps a civil date-time to one instant. During a
// fall-back overlap it chooses the first occurrence. During a spring-forward
// gap it chooses the first valid instant after the gap. Both choices keep the
// readiness gate monotonic once it opens.
func resolveLocalSchedule(value, layout string, location *time.Location) (time.Time, error) {
	wall, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse local scheduled_on %q: %w", value, err)
	}

	// time.Date returns one side of a skipped or repeated local time, but does
	// not specify which side. Use it only to find the adjacent zone boundaries,
	// then evaluate both offsets and choose a deterministic result below.
	seed := time.Date(
		wall.Year(), wall.Month(), wall.Day(),
		wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(),
		location,
	)
	intervalStart, intervalEnd := seed.ZoneBounds()
	transitions := make([]time.Time, 0, 2)
	for _, transition := range []time.Time{intervalStart, intervalEnd} {
		if !transition.IsZero() {
			transitions = append(transitions, transition)
		}
	}

	offsets := make(map[int]struct{}, 3)
	addOffset := func(instant time.Time) {
		_, offset := instant.In(location).Zone()
		offsets[offset] = struct{}{}
	}
	addOffset(seed)
	for _, transition := range transitions {
		addOffset(transition.Add(-time.Nanosecond))
		addOffset(transition)
	}

	var earliest time.Time
	for offset := range offsets {
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		if candidate.In(location).Format(layout) != value {
			continue
		}
		if earliest.IsZero() || candidate.Before(earliest) {
			earliest = candidate
		}
	}
	if !earliest.IsZero() {
		return earliest, nil
	}

	for _, transition := range transitions {
		before := wallClock(transition.Add(-time.Nanosecond), location)
		after := wallClock(transition, location)
		if after.After(before) && wall.After(before) && wall.Before(after) {
			return transition, nil
		}
	}

	return time.Time{}, fmt.Errorf("resolve local scheduled_on %q in %q", value, location)
}

func wallClock(instant time.Time, location *time.Location) time.Time {
	local := instant.In(location)
	return time.Date(
		local.Year(), local.Month(), local.Day(),
		local.Hour(), local.Minute(), local.Second(), local.Nanosecond(),
		time.UTC,
	)
}

func classifyScheduledOn(value string) (scheduledOnKind, string, time.Time, error) {
	if _, err := time.Parse(time.DateOnly, value); err == nil {
		return scheduledOnDate, time.DateOnly, time.Time{}, nil
	}
	for _, layout := range []string{localMinuteLayout, localSecondLayout} {
		if _, err := time.Parse(layout, value); err == nil {
			return scheduledOnLocalTime, layout, time.Time{}, nil
		}
	}
	if strings.HasSuffix(value, "Z") {
		if instant, err := time.Parse(time.RFC3339, value); err == nil {
			return scheduledOnInstant, "", instant, nil
		}
	}
	return 0, "", time.Time{}, fmt.Errorf(
		"scheduled_on %q must match YYYY-MM-DD, local YYYY-MM-DDTHH:MM[:SS], or RFC 3339 UTC with Z",
		value,
	)
}

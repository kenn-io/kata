package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// ScheduledReachedEventType records that an open issue's scheduled_on was
// reached, so adapters can start date-driven work from the event stream.
const ScheduledReachedEventType = "issue.scheduled_reached"

// scheduledReachedFloorKey is the meta row holding the first reached-schedule
// sweep time for this database.
const scheduledReachedFloorKey = "scheduled_reached_floor"

// EnsureScheduledReachedFloorTx stores at as the floor unless one exists, and
// returns the stored floor.
func EnsureScheduledReachedFloorTx(ctx context.Context, tx *sql.Tx, at time.Time) (time.Time, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2) ON CONFLICT(key) DO NOTHING`,
		scheduledReachedFloorKey, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return time.Time{}, fmt.Errorf("store scheduled reached floor: %w", err)
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=$1`, scheduledReachedFloorKey).Scan(&raw); err != nil {
		return time.Time{}, fmt.Errorf("read scheduled reached floor: %w", err)
	}
	floor, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse scheduled reached floor: %w", err)
	}
	return floor, nil
}

// PlanScheduledReached returns the reached scheduled_on date to record, or nil
// when the date is absent, still in the future, or its raw value was already
// recorded. Like the inbox sweep, the raw value is the delivery identity, so a
// date moved away and back is not reached twice.
//
// A date is recorded only when its instant or the edit that set its current
// value is at or after floor, the time this database first swept for reached
// schedules. Overdue schedules from before an upgrade therefore stay silent.
// A zero floor records every reached date.
func PlanScheduledReached(current jsontext.Value, now, floor time.Time, defaultTimezone string, history []Event) (*IssuePlanningDate, error) {
	value, timezone, instant, present, err := metadata.ScheduleFieldInstant(string(current), "scheduled_on", defaultTimezone)
	if err != nil {
		return nil, fmt.Errorf("evaluate scheduled_on: %w", err)
	}
	if !present || instant.After(now) {
		return nil, nil
	}
	for _, event := range history {
		if event.Type != ScheduledReachedEventType {
			continue
		}
		var recorded IssuePlanningDate
		if json.Unmarshal([]byte(event.Payload), &recorded) == nil && recorded.Value == value {
			return nil, nil
		}
	}
	if instant.Before(floor) && scheduledValueSetAt(history, value).Before(floor) {
		return nil, nil
	}
	return &IssuePlanningDate{Field: "scheduled_on", Value: value, Timezone: timezone, Instant: instant}, nil
}

// scheduledValueSetAt returns when the latest edit set scheduled_on to value,
// or the zero time when no edit did. A snapshot copies an earlier value, so its
// time is not an edit time.
func scheduledValueSetAt(history []Event, value string) time.Time {
	var setValue string
	var setAt time.Time
	ordered := append([]Event(nil), history...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return compareClock(eventClock(ordered[i]), eventClock(ordered[j])) < 0
	})
	for _, event := range ordered {
		var payload map[string]jsontext.Value
		if json.Unmarshal([]byte(event.Payload), &payload) != nil {
			continue
		}
		switch event.Type {
		case "issue.created":
			var values map[string]jsontext.Value
			if json.Unmarshal(payload["metadata"], &values) == nil {
				setValue = scheduledString(values["scheduled_on"])
			} else {
				setValue = ""
			}
			setAt = event.CreatedAt
		case "issue.snapshot":
			var values map[string]jsontext.Value
			if json.Unmarshal(payload["metadata"], &values) != nil {
				continue
			}
			snapshotValue := scheduledString(values["scheduled_on"])
			if snapshotValue != setValue {
				setAt = time.Time{}
			}
			setValue = snapshotValue
		case "issue.metadata_updated":
			var diff map[string]struct {
				To jsontext.Value `json:"to"`
			}
			if json.Unmarshal(payload["diff"], &diff) != nil {
				continue
			}
			if change, ok := diff["scheduled_on"]; ok {
				setValue, setAt = scheduledString(change.To), event.CreatedAt
			}
		case "recurrence.materialized":
			occurrence := scheduledString(payload["occurrence_key"])
			if occurrence != "" {
				setValue, setAt = occurrence, event.CreatedAt
			}
		}
	}
	if setValue != value {
		return time.Time{}
	}
	return setAt
}

func eventClock(event Event) FoldClock {
	return FoldClock{
		HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
		OriginInstanceUID: event.OriginInstanceUID, EventUID: event.UID,
	}
}

func scheduledString(raw jsontext.Value) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

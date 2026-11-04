package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"time"
)

// Normalize returns the document a typed write persists, retaining decoded
// values on validation failure and returning no document on decode failure.
func Normalize[T interface{ Validate() error }](raw []byte, decode func([]byte) (T, error)) (T, jsontext.Value, error) {
	var zero T
	if jsontext.Value(raw).Kind() != '{' {
		return zero, nil, invalid("definition must be a JSON object")
	}
	value, err := decode(raw)
	if err != nil {
		return zero, nil, err
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return value, nil, err
	}
	return value, normalized, value.Validate()
}

// ScheduleFire gives one instant and its representation in the trigger zone.
type ScheduleFire struct {
	At    time.Time `json:"at"`
	Local string    `json:"local"`
}

// SchedulePreview describes hypothetical trigger times, not dispatch promises.
type SchedulePreview struct {
	Delivery   string                `json:"delivery"`
	Basis      string                `json:"basis"`
	Zone       string                `json:"zone"`
	From       time.Time             `json:"from"`
	Next       []ScheduleFire        `json:"next"`
	Collisions *ValidationCollisions `json:"collisions,omitempty"`
}

// PreviewSchedule uses native progress when supplied, otherwise models creation
// at from. Callers bound count and validate the job and retained progress before previewing.
// Paused jobs show the same hypothetical instants as enabled jobs.
func PreviewSchedule(job JobDefinition, stored *RecurringProgress, from time.Time, count int) (SchedulePreview, error) {
	if _, err := from.UTC().MarshalText(); err != nil {
		return SchedulePreview{}, invalid("from must represent an RFC3339 UTC instant")
	}
	zone := TriggerLocation(job.Trigger)
	out := SchedulePreview{Delivery: "adapter", Basis: "none", Zone: zone.String(), From: from.UTC(), Next: []ScheduleFire{}}
	var progress RecurringProgress
	switch {
	case IsRecurringIssueJob(job):
		out.Delivery, out.Basis = "daemon", "fresh_anchor"
		progress = RecurringProgress{Version: 1, Anchor: from.UTC(), Through: from.UTC()}
		if stored != nil {
			progress, out.Basis = *stored, "stored_progress"
		}
	case job.Trigger.Kind == "interval":
		out.Basis = "adapter_defined"
	case job.Trigger.Kind == "cron":
		out.Basis = "adapter_trigger"
		schedule, err := adapterCronSchedule(job.Trigger)
		if err != nil {
			return out, err
		}
		if schedule == nil {
			out.Basis = "adapter_defined"
		}
	case job.Trigger.Kind == "once":
		out.Basis = "adapter_trigger"
	}
	job.Enabled = true
	times, err := recurringOccurrences(job, progress, from, count)
	if err != nil {
		return out, err
	}
	for _, at := range times {
		if _, err := at.UTC().MarshalText(); err != nil {
			continue
		}
		local, err := at.In(zone).MarshalText()
		if err != nil {
			// A UTC instant near a year boundary may fall outside RFC3339's
			// representable years in the trigger zone.
			continue
		}
		out.Next = append(out.Next, ScheduleFire{At: at, Local: string(local)})
	}
	return out, nil
}

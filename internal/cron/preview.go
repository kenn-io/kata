package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"time"
)

// NormalizeJob returns the same document a typed write would persist. A failed
// decode has no normalized document; decoded but invalid values retain theirs.
func NormalizeJob(raw []byte) (JobDefinition, jsontext.Value, error) {
	return normalizeDefinition(raw, DecodeJob)
}

// NormalizeWorkflow is NormalizeJob for a reusable workflow.
func NormalizeWorkflow(raw []byte) (WorkflowDefinition, jsontext.Value, error) {
	return normalizeDefinition(raw, DecodeWorkflow)
}

func normalizeDefinition[T interface{ Validate() error }](raw []byte, decode func([]byte) (T, error)) (T, jsontext.Value, error) {
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
	Delivery string         `json:"delivery"`
	Basis    string         `json:"basis"`
	Zone     string         `json:"zone"`
	From     time.Time      `json:"from"`
	Next     []ScheduleFire `json:"next"`
}

// PreviewSchedule uses native progress when supplied, otherwise models creation
// at from. Paused jobs show the same hypothetical instants as enabled jobs.
func PreviewSchedule(job JobDefinition, stored *RecurringProgress, from time.Time, count int) (SchedulePreview, error) {
	if count < 1 || count > 100 {
		return SchedulePreview{}, invalid("count must be between 1 and 100")
	}
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
		_, expression, _, _ := splitCronDirective(job.Trigger.Cron)
		if strings.HasPrefix(expression, "@every") {
			out.Basis = "adapter_defined"
		}
	case job.Trigger.Kind == "once":
		out.Basis = "adapter_trigger"
	}
	job.Enabled = true
	times, err := recurringOccurrences(job, progress, from, count, stored == nil)
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

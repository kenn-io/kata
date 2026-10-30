package cron

import (
	"encoding/json/v2"
	"strings"
	"time"
	"unicode/utf8"
)

// IsRecurringIssueJob recognizes native ownership even for disabled definitions.
func IsRecurringIssueJob(j JobDefinition) bool {
	return j.Action.Kind == "notify" && j.Issue != nil && j.Issue.Kind == "per-run"
}

// recurringTitleSuffix is the longest occurrence suffix a child title gains.
const recurringTitleSuffix = " — 2006-01-02T15:04:05.999999999Z"

func validateRecurring(j JobDefinition) error {
	p := j.Issue
	if !validUID(p.ParentUID) || p.UID != "" || strings.TrimSpace(p.Title) == "" || utf8.RuneCountInString(p.Title+recurringTitleSuffix) > 1024 {
		return invalid("native recurring issues require a parent UID and a bounded title")
	}
	if p.ScheduledOffsetSeconds != nil || p.DeadlineOffsetSeconds != nil || j.Overlap != "allow" || j.TimeoutSeconds != 0 || len(j.SecretRefs) != 0 {
		return invalid("native recurring issues require overlap=allow and cannot carry offsets or execution fields")
	}
	if len(j.Options) > 0 {
		var options map[string]any
		if err := json.Unmarshal(j.Options, &options); err != nil || options == nil || len(options) != 0 {
			return invalid("native recurring issues cannot carry adapter options")
		}
	}
	if j.Catchup == "skip" && j.GraceSeconds <= 0 {
		return invalid("native skip requires positive grace_seconds")
	}
	if j.Trigger.Kind != "cron" && j.Trigger.Kind != "interval" {
		return invalid("native recurring issues require cron or interval")
	}
	// The daemon sweeps every 30 seconds; shorter intervals cannot arrive on
	// time and only create bursts of issues.
	if j.Trigger.Kind == "interval" && j.Trigger.IntervalSeconds < 60 {
		return invalid("native recurring intervals must be at least 60 seconds")
	}
	_, err := recurringSchedule(j.Trigger)
	return err
}

func recurringSchedule(t Trigger) (func(time.Time) time.Time, error) {
	if t.Kind == "interval" {
		return nil, nil
	}
	expression := strings.TrimSpace(t.Cron)
	zone := t.Timezone
	if zone == "" {
		zone = "UTC"
	}
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		directive, rest, ok := strings.Cut(expression, " ")
		if !ok {
			return nil, invalid("invalid timezone directive")
		}
		_, embedded, _ := strings.Cut(directive, "=")
		if embedded != zone {
			return nil, invalid("conflicting cron timezone")
		}
		expression = strings.TrimSpace(rest)
	}
	if strings.HasPrefix(expression, "@every") {
		return nil, invalid("use an interval trigger instead of @every")
	}
	schedule, err := ParseCron("CRON_TZ=" + zone + " " + expression)
	if err != nil {
		return nil, invalid("invalid native cron expression")
	}
	return schedule.Next, nil
}

// RecurringProgress is replayable system metadata on a job's parent issue.
type RecurringProgress struct {
	Version int            `json:"version"`
	Anchor  time.Time      `json:"anchor"`
	Through time.Time      `json:"through"`
	Scan    *RecurringScan `json:"scan,omitempty"`
}

// RecurringScan freezes a latest-catchup horizon while enumerating a large backlog.
type RecurringScan struct {
	DefinitionEventUID string    `json:"definition_event_uid"`
	Until              time.Time `json:"until"`
	Cursor             time.Time `json:"cursor"`
}

// RecurringBatch is one bounded planner step. More requests a later step.
type RecurringBatch struct {
	Due      []time.Time
	Progress RecurringProgress
	More     bool
}

// Validate rejects unusable checkpoints instead of silently starting over.
func (p RecurringProgress) Validate() error {
	if p.Version != 1 || p.Anchor.IsZero() || p.Through.Before(p.Anchor) || p.Anchor.Location() != time.UTC || p.Through.Location() != time.UTC || p.Through.Year() > 9999 {
		return invalid("invalid recurring progress")
	}
	if s := p.Scan; s != nil && (!validUID(s.DefinitionEventUID) || s.Cursor.Before(p.Through) || s.Until.Before(s.Cursor) || s.Until.Location() != time.UTC || s.Cursor.Location() != time.UTC) {
		return invalid("invalid recurring scan")
	}
	return nil
}

// PlanRecurring evaluates one bounded step against an explicit clock and revision.
func PlanRecurring(j JobDefinition, definitionEventUID string, p RecurringProgress, now time.Time) (RecurringBatch, error) {
	out := RecurringBatch{Progress: p}
	if !IsRecurringIssueJob(j) || !validUID(definitionEventUID) {
		return out, invalid("native job and definition revision required")
	}
	if err := j.Validate(); err != nil {
		return out, err
	}
	if err := p.Validate(); err != nil {
		return out, err
	}
	now = now.UTC()
	if now.IsZero() || now.Year() > 9999 {
		return out, invalid("invalid recurring clock")
	}
	if !j.Enabled || !now.After(p.Through) {
		return out, nil
	}
	if p.Scan != nil && (p.Scan.DefinitionEventUID != definitionEventUID || j.Catchup != "latest") {
		p.Scan = nil
		out.Progress = p
	}
	next, err := recurringSchedule(j.Trigger)
	if err != nil {
		return out, err
	}
	if j.Trigger.Kind == "interval" {
		step := time.Duration(j.Trigger.IntervalSeconds) * time.Second
		next = func(cursor time.Time) time.Time {
			elapsed := cursor.Sub(p.Anchor)
			// Saturating time.Sub must never masquerade as a different occurrence.
			if elapsed == time.Duration(1<<63-1) {
				return time.Time{}
			}
			remaining := step - elapsed%step
			return cursor.Add(remaining).UTC()
		}
		if j.Catchup == "latest" {
			elapsed := now.Sub(p.Anchor)
			if elapsed == time.Duration(1<<63-1) {
				return out, invalid("interval horizon exceeds supported duration")
			}
			n := elapsed / step
			if n == 0 {
				return out, nil
			}
			due := p.Anchor.Add(n * step).UTC()
			if !due.After(p.Through) {
				return out, nil
			}
			out.Due = []time.Time{due}
			p.Through = now
			p.Scan = nil
			out.Progress = p
			return out, nil
		}
	}
	if j.Catchup == "skip" {
		cutoff := now.Add(-time.Duration(j.GraceSeconds) * time.Second)
		// Fast-forward only past an occurrence being discarded; an idle sweep
		// must not rewrite progress. Retain an occurrence exactly on grace.
		if first := next(p.Through); !first.IsZero() && first.Before(cutoff) {
			p.Through = cutoff.Add(-time.Nanosecond)
		}
	}
	cursor := p.Through
	until := now
	if j.Catchup == "latest" && p.Scan != nil {
		cursor = p.Scan.Cursor
		until = p.Scan.Until
		if now.Before(until) {
			out.More = true
			return out, nil
		}
	}
	consumed := 0
	for consumed < 1000 && len(out.Due) < 50 {
		due := next(cursor)
		if due.IsZero() || !due.After(cursor) || due.Year() > 9999 {
			return RecurringBatch{Progress: out.Progress}, invalid("schedule has no supported next occurrence")
		}
		if due.After(until) {
			if j.Catchup == "latest" && cursor.After(p.Through) {
				out.Due = []time.Time{cursor}
			}
			if consumed > 0 || p.Scan != nil || !p.Through.Equal(out.Progress.Through) {
				p.Through = until
				p.Scan = nil
				out.Progress = p
			}
			return out, nil
		}
		cursor = due.UTC()
		consumed++
		switch j.Catchup {
		case "all":
			out.Due = append(out.Due, cursor)
		case "skip":
			if !cursor.Before(now.Add(-time.Duration(j.GraceSeconds) * time.Second)) {
				out.Due = append(out.Due, cursor)
			}
		}
	}
	more := next(cursor)
	if more.IsZero() || !more.After(cursor) {
		return RecurringBatch{Progress: out.Progress}, invalid("schedule has no supported next occurrence")
	}
	out.More = !more.After(until)
	if j.Catchup == "latest" {
		if out.More {
			p.Scan = &RecurringScan{DefinitionEventUID: definitionEventUID, Until: until, Cursor: cursor}
		} else {
			out.Due = []time.Time{cursor}
			p.Through = until
			p.Scan = nil
		}
	} else {
		p.Through = cursor
		if !out.More {
			p.Through = until
		}
		p.Scan = nil
	}
	out.Progress = p
	return out, nil
}

// RecurringBacklog counts the children that sweeping from p up to now would
// deliver. The 1000-per-step cap only spreads a large backlog over several
// sweeps, so the count follows the planner to the end and is exact.
func RecurringBacklog(j JobDefinition, definitionEventUID string, p RecurringProgress, now time.Time) (int, error) {
	total := 0
	for {
		batch, err := PlanRecurring(j, definitionEventUID, p, now)
		if err != nil {
			return 0, err
		}
		total += len(batch.Due)
		if !batch.More {
			return total, nil
		}
		if sameRecurringPosition(p, batch.Progress) {
			return 0, invalid("recurring planner made no progress")
		}
		p = batch.Progress
	}
}

func sameRecurringPosition(a, b RecurringProgress) bool {
	if !a.Through.Equal(b.Through) || (a.Scan == nil) != (b.Scan == nil) {
		return false
	}
	return a.Scan == nil || a.Scan.Cursor.Equal(b.Scan.Cursor)
}

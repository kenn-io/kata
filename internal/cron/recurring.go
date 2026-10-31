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
	_, err := recurringNext(j.Trigger, time.Time{})
	return err
}

// recurringNext returns the step the daemon uses to find the occurrence after
// a cursor. Interval occurrences sit on whole steps from anchor.
func recurringNext(t Trigger, anchor time.Time) (func(time.Time) time.Time, error) {
	if t.Kind == "interval" {
		step := time.Duration(t.IntervalSeconds) * time.Second
		return func(cursor time.Time) time.Time {
			elapsed := cursor.Sub(anchor)
			// Saturating time.Sub must never masquerade as a different occurrence.
			if elapsed == time.Duration(1<<63-1) {
				return time.Time{}
			}
			remaining := step - elapsed%step
			return cursor.Add(remaining).UTC()
		}, nil
	}
	zone := t.Timezone
	if zone == "" {
		zone = "UTC"
	}
	embedded, expression, has, bare := splitCronDirective(t.Cron)
	if bare {
		return nil, invalid("invalid timezone directive")
	}
	if has && embedded != zone {
		return nil, invalid("conflicting cron timezone")
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
	next, err := recurringNext(j.Trigger, p.Anchor)
	if err != nil {
		return out, err
	}
	if j.Trigger.Kind == "interval" {
		step := time.Duration(j.Trigger.IntervalSeconds) * time.Second
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

// RecurringOccurrences previews up to n instants after the later of after and
// the job's delivered progress, stepping as PlanRecurring does. A paused job has
// none. Kata holds no anchor or catch-up for an adapter job, so only its cron and
// once triggers have instants (the adapter's own trigger times); progress is
// read for native jobs only.
func RecurringOccurrences(j JobDefinition, progress RecurringProgress, after time.Time, n int) ([]time.Time, error) {
	if !j.Enabled || n <= 0 {
		return nil, nil
	}
	after = after.UTC()
	var next func(time.Time) time.Time
	switch {
	case IsRecurringIssueJob(j):
		if err := j.Validate(); err != nil {
			return nil, err
		}
		if err := progress.Validate(); err != nil {
			return nil, err
		}
		after = laterOf(after, progress.Through)
		step, err := recurringNext(j.Trigger, progress.Anchor)
		if err != nil {
			return nil, err
		}
		next = step
	case j.Trigger.Kind == "cron":
		step, err := adapterCronNext(j.Trigger)
		if err != nil || step == nil {
			return nil, err
		}
		next = step
	case j.Trigger.Kind == "once":
		at, err := time.Parse(time.RFC3339, j.Trigger.At)
		if err != nil {
			return nil, invalid("once requires an RFC3339 instant")
		}
		if at = at.UTC(); at.After(after) {
			return []time.Time{at}, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
	var out []time.Time
	for cursor := after; len(out) < n; {
		due := next(cursor)
		if due.IsZero() || !due.After(cursor) || due.Year() > 9999 {
			break
		}
		out = append(out, due.UTC())
		cursor = due
	}
	return out, nil
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// splitCronDirective separates a leading TZ= or CRON_TZ= directive from a
// cron expression, returning the directive's zone name. has reports whether a
// directive was present, since its zone may be empty; bare reports a directive
// with no schedule after it.
func splitCronDirective(expression string) (zone, rest string, has, bare bool) {
	expression = strings.TrimSpace(expression)
	if strings.HasPrefix(expression, "TZ=") || strings.HasPrefix(expression, "CRON_TZ=") {
		directive, rest, ok := strings.Cut(expression, " ")
		if !ok {
			return "", "", true, true
		}
		_, zone, _ = strings.Cut(directive, "=")
		return zone, strings.TrimSpace(rest), true, false
	}
	return "", expression, false, false
}

// adapterCronNext steps an adapter's cron trigger with kata's cron grammar: the
// trigger timezone applies only when the expression has no directive of its
// own, and an @every descriptor is an interval with no instant (nil step).
func adapterCronNext(t Trigger) (func(time.Time) time.Time, error) {
	_, rest, has, _ := splitCronDirective(t.Cron)
	if strings.HasPrefix(rest, "@every") {
		return nil, nil
	}
	expression := strings.TrimSpace(t.Cron)
	if !has && t.Timezone != "" {
		expression = "CRON_TZ=" + t.Timezone + " " + expression
	}
	schedule, err := ParseCron(expression)
	if err != nil {
		return nil, invalid("invalid cron expression")
	}
	return schedule.Next, nil
}

// TriggerLocation is the zone a trigger's wall-clock times are written in: the
// expression's own directive, else the trigger timezone, else UTC.
func TriggerLocation(t Trigger) *time.Location {
	zone, _, _, _ := splitCronDirective(t.Cron)
	if zone == "" {
		zone = t.Timezone
	}
	if loc, err := time.LoadLocation(zone); err == nil && zone != "" {
		return loc
	}
	return time.UTC
}

package cron

import (
	"time"

	robcron "github.com/robfig/cron/v3"
)

// dstNext uses robfig only for naive wall-clock matching. Its zoned hour
// arithmetic can skip entire days at half-hour offset changes.
func dstNext(spec *robcron.SpecSchedule) func(time.Time) time.Time {
	naive := *spec
	naive.Location = time.UTC
	fixed := spec.Hour&((1<<24)-1) != (1<<24)-1
	return func(cursor time.Time) time.Time {
		loc := spec.Location
		if loc == time.Local {
			loc = cursor.Location()
		}
		due, _ := dstOccurrence(&naive, fixed, loc, cursor)
		return due
	}
}

// dstOccurrence also returns the number of offset segments visited, so the
// bounded search can be checked without a mutable instrumentation hook.
func dstOccurrence(naive *robcron.SpecSchedule, fixed bool, loc *time.Location, cursor time.Time) (time.Time, int) {
	lb, at := cursor.UTC(), cursor.UTC()
	limit := cursor.AddDate(5, 0, 0)
	if fixed {
		// A restored cursor may already be in the second pass of a repeated hour.
		back := cursor.Add(-24 * time.Hour)
		if delta := zoneOffset(back, loc) - zoneOffset(cursor, loc); delta > 0 {
			transition := zoneTransition(back, cursor, loc)
			if skip := transition.Add(delta - time.Nanosecond); skip.After(lb) {
				lb = skip
			}
		}
	}
	for i := 1; i <= 128; i++ {
		before := zoneOffset(at, loc)
		end := zoneSegmentEnd(at, loc)
		wall := naive.Next(lb.Add(before))
		if wall.IsZero() {
			return time.Time{}, i
		}
		due := wall.Add(-before)
		if end.IsZero() || due.Before(end) {
			if due.After(limit) {
				return time.Time{}, i
			}
			return due.UTC(), i
		}
		if end.After(limit) {
			return time.Time{}, i
		}
		after := zoneOffset(end, loc)
		if fixed && after > before {
			first := naive.Next(end.UTC().Add(before - time.Nanosecond))
			if !first.IsZero() && first.Before(end.UTC().Add(after)) {
				return end.UTC(), i
			}
		}
		lb, at = end.Add(-time.Nanosecond).UTC(), end
		if fixed && before > after {
			lb = end.Add(before - after - time.Nanosecond).UTC()
		}
	}
	// Fail closed through the planner's unsupported-occurrence error rather
	// than holding a delivery transaction in an unbounded timezone loop.
	return time.Time{}, 128
}

func zoneOffset(at time.Time, loc *time.Location) time.Duration {
	_, seconds := at.In(loc).Zone()
	return time.Duration(seconds) * time.Second
}

// Go's synthesized ZoneBounds can end at/before at on leap-year December 31
// beyond the explicit tzdata table. Always advance, even at synthetic bounds
// where the offset does not actually change.
func zoneSegmentEnd(at time.Time, loc *time.Location) time.Time {
	_, end := at.In(loc).ZoneBounds()
	if end.IsZero() || end.After(at) {
		return end
	}
	probe := at.Add(24 * time.Hour)
	if zoneOffset(probe, loc) != zoneOffset(at, loc) {
		return zoneTransition(at, probe, loc)
	}
	return probe
}

// zoneTransition finds the first changed offset in (a,b], to the nanosecond.
// Callers bracket one transition with different offsets.
func zoneTransition(a, b time.Time, loc *time.Location) time.Time {
	offset := zoneOffset(a, loc)
	for b.Sub(a) > time.Nanosecond {
		mid := a.Add(b.Sub(a) / 2)
		if zoneOffset(mid, loc) == offset {
			a = mid
		} else {
			b = mid
		}
	}
	return b
}

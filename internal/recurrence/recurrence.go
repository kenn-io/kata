// Package recurrence wraps github.com/teambition/rrule-go behind a small,
// date-string-oriented API. Rules are RFC-5545 RRULE strings; DTSTART
// and "after" are YYYY-MM-DD dates interpreted in the rule's IANA timezone.
package recurrence

import (
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
)

const dateFmt = "2006-01-02"

// Walk returns the next occurrence date strictly after `after`.
// Returns (nil, nil) when the series is exhausted (UNTIL/COUNT reached).
// Returns (nil, err) when the rule cannot be parsed.
func Walk(rule, dtstart, tz, after string) (*string, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", tz, err)
	}
	dt, err := time.ParseInLocation(dateFmt, dtstart, loc)
	if err != nil {
		return nil, fmt.Errorf("parse dtstart %q: %w", dtstart, err)
	}
	afterDate, err := time.ParseInLocation(dateFmt, after, loc)
	if err != nil {
		return nil, fmt.Errorf("parse after %q: %w", after, err)
	}

	opt, err := rrule.StrToROption(rule)
	if err != nil {
		return nil, fmt.Errorf("parse rrule %q: %w", rule, err)
	}
	opt.Dtstart = dt
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, fmt.Errorf("build rrule: %w", err)
	}

	// Walk advances by date, not by sub-day occurrence. If the RRULE has
	// sub-day components (FREQ=HOURLY, BYHOUR, etc.), r.After may return a
	// time on the same calendar date as afterDate. Skip any same-date (or
	// earlier) results by advancing until we get a strictly-later date or
	// the series is exhausted.
	afterFormatted := afterDate.Format(dateFmt)
	n := r.After(afterDate, false)
	for !n.IsZero() {
		cand := n.In(loc).Format(dateFmt)
		if cand > afterFormatted {
			// cand > afterFormatted is a safe lexicographic comparison for
			// YYYY-MM-DD strings — equivalent to a calendar-date comparison.
			return &cand, nil
		}
		// Same day or earlier — advance past this sub-day occurrence.
		n = r.After(n, false)
	}
	return nil, nil
}

// Next returns the first occurrence on or after dtstart — useful when
// materializing the initial instance of a fresh recurrence.
func Next(rule, dtstart, tz string) (*string, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	dt, err := time.ParseInLocation(dateFmt, dtstart, loc)
	if err != nil {
		return nil, err
	}
	prev := dt.AddDate(0, 0, -1).Format(dateFmt)
	return Walk(rule, dtstart, tz, prev)
}

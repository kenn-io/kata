// Package recurrence wraps github.com/teambition/rrule-go behind a small,
// date-string-oriented API. Rules are RFC-5545 RRULE strings; DTSTART
// and "after" are YYYY-MM-DD dates interpreted in the rule's IANA timezone.
package recurrence

import (
	"context"
	"fmt"
	"time"

	"github.com/teambition/rrule-go"
)

const dateFmt = "2006-01-02"

// Walk returns the next occurrence date strictly after `after`.
// Returns (nil, nil) when the series is exhausted (UNTIL/COUNT reached).
// Returns (nil, err) when the rule cannot be parsed.
func Walk(rule, dtstart, tz, after string) (*string, error) {
	return WalkContext(context.Background(), rule, dtstart, tz, after)
}

// WalkContext stops searching when ctx is canceled.
func WalkContext(ctx context.Context, rule, dtstart, tz, after string) (*string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	// Iterate once so cancellation can interrupt high-frequency rules while
	// skipping occurrences on the same date.
	afterFormatted := afterDate.Format(dateFmt)
	next := r.Iterator()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, ok := next()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		cand := n.In(loc).Format(dateFmt)
		if cand > afterFormatted {
			return &cand, nil
		}
	}
}

// Next returns the first occurrence on or after dtstart — useful when
// materializing the initial instance of a fresh recurrence.
func Next(rule, dtstart, tz string) (*string, error) {
	return NextContext(context.Background(), rule, dtstart, tz)
}

// NextContext stops searching when ctx is canceled.
func NextContext(ctx context.Context, rule, dtstart, tz string) (*string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	dt, err := time.ParseInLocation(dateFmt, dtstart, loc)
	if err != nil {
		return nil, err
	}
	prev := dt.AddDate(0, 0, -1).Format(dateFmt)
	return WalkContext(ctx, rule, dtstart, tz, prev)
}

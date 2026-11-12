// Package timeparse parses relative or absolute time specifications.
package timeparse

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

var dayQuantity = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)$`)

// Since resolves a duration relative to anchor, or parses an RFC3339 instant.
func Since(s string, anchor time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	invalid := func() (time.Time, error) {
		return time.Time{}, fmt.Errorf("invalid time spec %q: must be a duration (24h, 7d) or RFC3339 timestamp", s)
	}
	expanded := s
	days := false
	if before, ok := strings.CutSuffix(s, "d"); ok {
		if !dayQuantity.MatchString(before) {
			return invalid()
		}
		expanded = before + "h"
		days = true
	}
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return invalid()
	}
	if days {
		if d > time.Duration(math.MaxInt64/24) || d < time.Duration(math.MinInt64/24) {
			return invalid()
		}
		d *= 24
	}
	if d == time.Duration(math.MinInt64) {
		return invalid()
	}
	return anchor.Add(-d).UTC(), nil
}

// Window resolves a relative start against its end, using one sampled instant
// for both bounds when no end was supplied. Absolute starts retain the
// daemon's default end behavior.
func Window(since, until string, now time.Time) (string, string, error) {
	if since == "" {
		return since, until, nil
	}
	anchor := now
	if until != "" {
		var err error
		anchor, err = time.Parse(time.RFC3339, until)
		if err != nil {
			return "", "", fmt.Errorf("invalid until")
		}
	}
	_, absoluteErr := time.Parse(time.RFC3339, since)
	instant, err := Since(since, anchor)
	if err != nil {
		return "", "", err
	}
	if absoluteErr != nil && until == "" {
		until = anchor.UTC().Format(time.RFC3339Nano)
	}
	return instant.Format(time.RFC3339Nano), until, nil
}

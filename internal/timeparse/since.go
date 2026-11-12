// Package timeparse parses relative or absolute time specifications.
package timeparse

import (
	"fmt"
	"math"
	"strings"
	"time"
)

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

package githubsync

import (
	"fmt"
	"strings"
	"time"
)

// ParseSince parses an optional updated-after cutoff. Dates mean midnight UTC;
// fractional seconds are rejected because GitHub's REST cutoff uses seconds.
func ParseSince(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil || strings.ContainsAny(value, ".,") {
		return nil, fmt.Errorf("GitHub sync since must be YYYY-MM-DD or RFC3339 with whole seconds")
	}
	parsed = parsed.UTC()
	if parsed.Year() < 0 || parsed.Year() > 9999 {
		return nil, fmt.Errorf("GitHub sync since must remain a four-digit RFC3339 year in UTC")
	}
	return &parsed, nil
}

// SinceTime returns the optional configured updated-after cutoff.
func (c Config) SinceTime() (*time.Time, error) { return ParseSince(c.Since) }

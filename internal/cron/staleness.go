package cron

import "time"

// RunStaleness is advisory evidence age, derived on reads and never persisted.
type RunStaleness struct {
	Stale   bool      `json:"stale"`
	StaleAt time.Time `json:"stale_at"`
	Basis   string    `json:"basis" enum:"timeout_seconds,default"`
}

// Staleness evaluates running evidence using the current job timeout plus a
// 15-minute reporting margin, or 24 hours when no positive timeout is set.
// updatedAt must be an RFC3339 instant. Deadlines saturate at the last RFC3339
// instant rather than overflowing durations or producing an unserializable year.
func Staleness(status string, updatedAt time.Time, timeoutSeconds int64, now time.Time) *RunStaleness {
	if status != "running" {
		return nil
	}
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	deadline, basis := last, "default"
	remaining := last.Unix() - updatedAt.Unix()
	seconds := int64(86400)
	if timeoutSeconds > 0 {
		basis = "timeout_seconds"
		// Compare before adding the margin or converting seconds to a duration.
		if timeoutSeconds > remaining-900 {
			return &RunStaleness{Stale: !now.Before(last), StaleAt: last, Basis: basis}
		}
		seconds = timeoutSeconds + 900
	}
	if seconds <= remaining {
		deadline = time.Unix(updatedAt.Unix()+seconds, int64(updatedAt.Nanosecond())).UTC()
	}
	// An RFC3339 year-zero value with a positive offset may denote an instant
	// before the earliest UTC RFC3339 value, even after adding the timeout.
	first := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	if deadline.Before(first) {
		deadline = first
	}
	return &RunStaleness{Stale: !now.Before(deadline), StaleAt: deadline, Basis: basis}
}

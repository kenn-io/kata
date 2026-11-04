package cron

import "time"

// ApplyPassover excludes scheduled instants at or before cutoff while retaining
// the original interval anchor. A newly advanced cursor discards frozen scans.
func ApplyPassover(p RecurringProgress, cutoff time.Time) (RecurringProgress, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	if cutoff.IsZero() || cutoff.Location() != time.UTC || cutoff.Year() < 1 || cutoff.Year() > 9999 {
		return p, invalid("invalid passover cutoff")
	}
	if cutoff.After(p.Through) {
		p.Through = cutoff
		p.Scan = nil
	}
	return p, nil
}

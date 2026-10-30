package metadata

import "time"

// ValidCalendarDate reports whether value is a real calendar date written as
// YYYY-MM-DD. Callers share it so every date and zone input is judged alike.
func ValidCalendarDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

// ValidTimeZone reports whether name loads as a time zone.
func ValidTimeZone(name string) bool {
	_, err := time.LoadLocation(name)
	return err == nil
}

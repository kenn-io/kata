package issuesync

import "time"

// StatusObservation contains a verified provider read, separate from content.
// RawStatus distinguishes provider substates and a null Notion status.
type StatusObservation struct {
	RawStatus    *string
	Status       string
	ClosedReason string
	ClosedAt     *time.Time
	Version      time.Time
	Locator      string
}

// StatusError classifies delivery failures without displaying remote response
// bodies or transport diagnostics that could contain credentials.
type StatusError struct {
	Message    string
	HTTPStatus int
	RetryAfter time.Duration
	// RateLimited marks a provider rate limit whose HTTP status would otherwise read as another failure.
	RateLimited bool
	AuthFailure bool
	Ambiguous   bool
	Blocked     bool
	Cause       error
}

func (e *StatusError) Error() string { return e.Message }

func (e *StatusError) Unwrap() error { return e.Cause }

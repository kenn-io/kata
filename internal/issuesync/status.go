package issuesync

import "time"

// StatusObservation contains a verified provider read, separate from content.
// RawStatus distinguishes provider substates and a null Notion status.
type StatusObservation struct {
	RawStatus         *string
	Status            string
	ClosedReason      string
	ClosedAt          *time.Time
	Version           time.Time
	Locator           string
	SchemaFingerprint string
}

// StatusError classifies delivery failures without retaining remote response
// bodies or transport diagnostics that could contain credentials.
type StatusError struct {
	Message    string
	HTTPStatus int
	RetryAfter time.Duration
	Ambiguous  bool
	Blocked    bool
}

func (e *StatusError) Error() string { return e.Message }

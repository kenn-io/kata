package daemon

import (
	"context"
	"errors"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

// trackerClientError is the selected-account boundary: retain useful delivery
// classification, never raw upstream messages, bodies, URLs or credential paths.
func trackerClientError(err error) error {
	if err == nil {
		return nil
	}
	// A known uncertain effect must keep both its semantic sentinel and the
	// ambiguous delivery marker through provider/session wrapper boundaries.
	if errors.Is(err, db.ErrTrackerOutcomeUnknown) {
		return trackerUnknownOutcome(nil)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	for _, safe := range []error{tracker.ErrDenied, tracker.ErrUnavailable, db.ErrTrackerActivationRequired, db.ErrTrackerFenceChanged, db.ErrTrackerFenceInvalid, db.ErrIssueSyncBindingChanged, db.ErrIssueSyncNotEnabled, db.ErrIssueSyncAlreadyRunning} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	if classified, ok := errors.AsType[*issuesync.StatusError](err); ok {
		return &issuesync.StatusError{Message: "selected tracker request failed", HTTPStatus: classified.HTTPStatus, RetryAfter: classified.RetryAfter, RateLimited: classified.RateLimited, AuthFailure: classified.AuthFailure, Ambiguous: classified.Ambiguous, Blocked: classified.Blocked}
	}
	return tracker.ErrUnavailable
}
func trackerCall[T any](result T, err error) (T, error) { return result, trackerClientError(err) }
func trackerCallWithCursor[T, C any](result T, cursor C, err error) (T, C, error) {
	return result, cursor, trackerClientError(err)
}

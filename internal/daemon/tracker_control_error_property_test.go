package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
	"hegel.dev/go/hegel"
	"net/http"
	"testing"
)

// An actual uncertain effect dominates incidental error causes independently
// of join order, transparent wrapping and private diagnostic text.
func TestTrackerUnknownClassificationSurvivesErrorCompositionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		diagnostic := "fixture-private-" + hegel.Draw(ht, hegel.Text())
		causes := []error{context.DeadlineExceeded, context.Canceled, db.ErrNotFound, db.ErrIssueSyncNotEnabled, db.ErrIssueSyncAlreadyRunning, db.ErrIssueSyncProjectAlreadyBound, tracker.ErrInvalid, tracker.ErrDenied, errors.New(diagnostic), api.NewError(http.StatusBadRequest, "validation", diagnostic, "", nil)}
		other := hegel.Draw(ht, hegel.SampledFrom(causes))
		cause := errors.Join(db.ErrTrackerOutcomeUnknown, other)
		if hegel.Draw(ht, hegel.Booleans()) {
			cause = errors.Join(other, db.ErrTrackerOutcomeUnknown)
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			cause = fmt.Errorf("%s: %w", diagnostic, cause)
		}
		normalized := trackerClientError(cause)
		require.ErrorIs(ht, normalized, db.ErrTrackerOutcomeUnknown)
		var delivery *issuesync.StatusError
		require.ErrorAs(ht, normalized, &delivery)
		require.True(ht, delivery.Ambiguous)
		require.NotContains(ht, normalized.Error(), "fixture-private-")
		require.Equal(ht, "tracker_outcome_unknown", trackerControlErrorCode(cause))
		require.Equal(ht, "tracker_outcome_unknown", trackerControlErrorCode(normalized))
		var wire *api.APIError
		require.ErrorAs(ht, trackerAPIError(cause), &wire)
		require.Equal(ht, "tracker_outcome_unknown", wire.Code)
		require.NotContains(ht, wire.Message, "fixture-private-")
	}, hegel.WithTestCases(100))
}

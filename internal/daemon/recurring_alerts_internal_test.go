package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Deduplication suppresses only unchanged recurring failures. Ordinary errors
// retain their identity even when joined with a suppressed recurring error.
func TestRecurringAlertsPreserveIndependentFailures(t *testing.T) {
	recurring := errors.New("recurring failure")
	ordinary := errors.New("notification listing failed")
	require.NoError(t, recurringAlerts(recurringSweepError{err: recurring, report: false}))
	require.ErrorIs(t, recurringAlerts(recurringSweepError{err: recurring, report: true}), recurring)
	got := recurringAlerts(errors.Join(recurringSweepError{err: recurring, report: false}, ordinary))
	require.ErrorIs(t, got, ordinary)
	require.NotErrorIs(t, got, recurring)
	got = recurringAlerts(errors.Join(recurringSweepError{err: recurring, report: true}, ordinary))
	require.ErrorIs(t, got, ordinary)
	require.ErrorIs(t, got, recurring)
}

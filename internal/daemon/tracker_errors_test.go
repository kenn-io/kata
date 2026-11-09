package daemon

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
	"testing"
	"time"
)

func TestTrackerSelectedErrorsKeepClassificationWithoutDiagnostics(t *testing.T) {
	original := &issuesync.StatusError{Message: "fixture-private-diagnostic", Cause: errors.New("fixture-private-cause"), HTTPStatus: 429, RateLimited: true, Ambiguous: true, RetryAfter: time.Minute}
	sanitized := trackerClientError(original)
	var classified *issuesync.StatusError
	require.ErrorAs(t, sanitized, &classified)
	require.Equal(t, original.HTTPStatus, classified.HTTPStatus)
	require.True(t, classified.RateLimited)
	require.True(t, classified.Ambiguous)
	require.Equal(t, time.Minute, classified.RetryAfter)
	require.Nil(t, classified.Cause)
	require.NotContains(t, sanitized.Error(), "fixture-private")
	require.NotContains(t, trackerClientError(errors.New("fixture-private-diagnostic")).Error(), "fixture-private")
	require.ErrorIs(t, trackerClientError(context.Canceled), context.Canceled)
}

func TestTrackerSelectedErrorsPreserveSafeInternalFences(t *testing.T) {
	require.ErrorIs(t, trackerClientError(db.ErrTrackerActivationRequired), db.ErrTrackerActivationRequired)
	require.ErrorIs(t, trackerClientError(db.ErrIssueSyncBindingChanged), db.ErrIssueSyncBindingChanged)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/issuesync"
)

func TestOutcomesSyncTransitions(t *testing.T) {
	reporter := &fakeTelemetryReporter{}
	observe := NewSyncOutcomeObserver(reporter)
	for _, err := range []error{errors.New("failed"), errors.New("still failing"), nil, errors.New("failed again")} {
		observe("github", 1, err)
	}
	observe("github", 2, errors.New("second binding"))
	observe("linear", 1, &issuesync.StatusError{HTTPStatus: 401})
	observe("linear", 2, context.Canceled)
	require.Equal(t, []capturedTelemetryEvent{
		{event: "sync_failed", properties: map[string]any{"sync_source": "github", "reason": "other"}},
		{event: "sync_failed", properties: map[string]any{"sync_source": "github", "reason": "other"}},
		{event: "sync_failed", properties: map[string]any{"sync_source": "github", "reason": "other"}},
		{event: "sync_failed", properties: map[string]any{"sync_source": "linear", "reason": "auth"}},
	}, reporter.captured)
	observe("linear", 2, errors.New("failed after cancellation"))
	require.Len(t, reporter.captured, 5)
	require.Equal(t, capturedTelemetryEvent{
		event: "sync_failed", properties: map[string]any{"sync_source": "linear", "reason": "other"},
	}, reporter.captured[4])
}

func TestOutcomesSyncReasons(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{&issuesync.StatusError{HTTPStatus: 401}, "auth"}, {&issuesync.StatusError{HTTPStatus: 403}, "auth"},
		{&issuesync.StatusError{HTTPStatus: 429}, "rate_limit"}, {&issuesync.StatusError{RetryAfter: time.Minute}, "rate_limit"},
		{&issuesync.StatusError{HTTPStatus: 403, RateLimited: true}, "rate_limit"},
		{&issuesync.StatusError{HTTPStatus: 200, RetryAfter: time.Minute}, "rate_limit"},
		{&issuesync.StatusError{HTTPStatus: 503, RetryAfter: time.Minute}, "server"},
		{&issuesync.StatusError{HTTPStatus: 503, RetryAfter: time.Minute, Ambiguous: true}, "server"},
		{context.DeadlineExceeded, "timeout"}, {&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}, "network"},
		{errors.New("private error text"), "other"},
	} {
		t.Run(tc.reason+fmt.Sprint(tc.err), func(t *testing.T) {
			reporter := &fakeTelemetryReporter{}
			NewSyncOutcomeObserver(reporter)("github", 1, fmt.Errorf("wrapped: %w", tc.err))
			require.Equal(t, []capturedTelemetryEvent{{event: "sync_failed", properties: map[string]any{"sync_source": "github", "reason": tc.reason}}}, reporter.captured)
		})
	}
}

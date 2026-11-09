package daemon

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/issuesync"
	"hegel.dev/go/hegel"
)

// Contract: changing a provider diagnostic or its error chain cannot change the
// public delivery classification, nor expose that diagnostic on the wire.
func TestTrackerWireFailureDiagnosticNoninterferenceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.Integers[int32](math.MinInt32, math.MaxInt32))
		retry := time.Duration(hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		rate := hegel.Draw(ht, hegel.Booleans())
		auth := hegel.Draw(ht, hegel.Booleans())
		ambiguous := hegel.Draw(ht, hegel.Booleans())
		blocked := hegel.Draw(ht, hegel.Booleans())
		sentinel := fmt.Sprintf("synthetic-private-diagnostic-%d", hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		first := &issuesync.StatusError{Message: sentinel, Cause: errors.New(sentinel), HTTPStatus: int(status), RetryAfter: retry, RateLimited: rate, AuthFailure: auth, Ambiguous: ambiguous, Blocked: blocked}
		second := *first
		second.Message, second.Cause = "different-private-diagnostic", errors.New("different-private-cause")
		a, err := json.Marshal(workerStatusFailure(first))
		require.NoError(ht, err)
		b, err := json.Marshal(workerStatusFailure(fmt.Errorf("wrapped: %w", &second)))
		require.NoError(ht, err)
		require.Equal(ht, string(a), string(b))
		require.NotContains(ht, string(a), sentinel)
		require.NotContains(ht, trackerWorkerError(trackerWorkerReply{Code: "unknown", Failure: workerStatusFailure(first)}).Error(), sentinel)
	}, hegel.WithTestCases(500))
}

// Contract: untrusted classification values are bounded before serialization;
// their exact class flags survive and no signed duration can overflow the wire.
func TestTrackerWireFailureClassificationBoundsProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		status := hegel.Draw(ht, hegel.Integers(-1000, 1000))
		retry := time.Duration(hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		original := &issuesync.StatusError{HTTPStatus: status, RetryAfter: retry}
		failure := workerStatusFailure(original)
		wantStatus := status
		if status < 100 || status > 599 {
			wantStatus = 0
		}
		wantRetry := retry
		if retry < 0 {
			wantRetry = 0
		}
		if retry > 24*time.Hour {
			wantRetry = 24 * time.Hour
		}
		require.Equal(ht, wantStatus, failure.HTTPStatus)
		require.Equal(ht, wantRetry.Milliseconds(), failure.RetryAfterMilliseconds)
		require.True(ht, failure.valid())
	}, hegel.WithTestCases(500))
}

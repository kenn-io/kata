package federation

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const authInvalidBody = `{"error":{"code":"auth_invalid","message":"federation token is not valid for this project"}}`

func refusedSyncError(status int) error {
	return fmt.Errorf("sync failed: %w", &HubStatusError{Path: "/api/v1/projects/42/federation/events", StatusCode: status, Body: authInvalidBody})
}

func mustSyncRetryIdentity(t testing.TB, binding db.FederationBinding, credential config.FederationCredential) [32]byte {
	t.Helper()
	identity, err := syncRetryIdentity(binding, credential)
	require.NoError(t, err)
	return identity
}

// noJitter keeps the schedule exact so the test can name every delay.
func noJitter(time.Duration) time.Duration { return 0 }

func TestSyncRetryRefusedBackoffDoublesToCap(t *testing.T) {
	retries := newSyncRetries(noJitter)
	identity := mustSyncRetryIdentity(t, db.FederationBinding{ProjectID: 1}, config.FederationCredential{Token: "token"})
	completed := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	want := []time.Duration{
		time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		15 * time.Minute, 15 * time.Minute, 15 * time.Minute,
	}
	for i, delay := range want {
		retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		require.True(t, retries.blocked(1, identity, completed.Add(delay-time.Nanosecond)), "attempt %d", i)
		require.False(t, retries.blocked(1, identity, completed.Add(delay)), "attempt %d", i)
		status, ok := retries.Status(1)
		require.True(t, ok)
		require.Equal(t, SyncRetryStatus{
			Reason: SyncRetryRefused, HTTPStatus: http.StatusForbidden,
			Failures: i + 1, NextAttemptAt: completed.Add(delay),
		}, status)
		completed = completed.Add(delay)
	}
}

func TestSyncRetryUnauthorizedIsRefused(t *testing.T) {
	retries := newSyncRetries(noJitter)
	completed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retries.record(1, [32]byte{}, refusedSyncError(http.StatusUnauthorized), 30*time.Second, completed)
	status, ok := retries.Status(1)
	require.True(t, ok)
	require.Equal(t, SyncRetryRefused, status.Reason)
	require.Equal(t, http.StatusUnauthorized, status.HTTPStatus)
	require.Equal(t, completed.Add(time.Minute), status.NextAttemptAt)
}

func TestSyncRetryJitterOnlyShortensTheDelay(t *testing.T) {
	var asked []time.Duration
	retries := newSyncRetries(func(limit time.Duration) time.Duration {
		asked = append(asked, limit)
		return limit - time.Nanosecond
	})
	completed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 6 {
		retries.record(1, [32]byte{}, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		status, _ := retries.Status(1)
		completed = status.NextAttemptAt
	}
	// Jitter takes up to a quarter off each delay; the doubling continues
	// from the unjittered delay.
	require.Equal(t, []time.Duration{
		15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute,
		15 * time.Minute / 4, 15 * time.Minute / 4,
	}, asked)
}

func TestSyncRetryUnavailableUsesShorterBackoff(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			retries := newSyncRetries(noJitter)
			completed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for i, delay := range []time.Duration{
				30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute,
			} {
				retries.record(1, [32]byte{}, &HubStatusError{StatusCode: code}, 30*time.Second, completed)
				status, ok := retries.Status(1)
				require.True(t, ok)
				require.Equal(t, SyncRetryStatus{Reason: SyncRetryUnavailable, HTTPStatus: code, Failures: i + 1, NextAttemptAt: completed.Add(delay)}, status)
				completed = completed.Add(delay)
			}
		})
	}
}

func TestSyncRetryHonorsRetryAfter(t *testing.T) {
	retries := newSyncRetries(noJitter)
	completed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	retries.record(1, [32]byte{}, &HubStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: 10 * time.Minute}, 30*time.Second, completed)
	status, _ := retries.Status(1)
	require.Equal(t, completed.Add(10*time.Minute), status.NextAttemptAt)

	// A shorter hint than the backoff does not shorten it.
	completed = status.NextAttemptAt
	retries.record(1, [32]byte{}, &HubStatusError{StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Second}, 30*time.Second, completed)
	status, _ = retries.Status(1)
	require.Equal(t, completed.Add(time.Minute), status.NextAttemptAt)

	// A hint beyond the bound waits only for the bound.
	completed = status.NextAttemptAt
	retries.record(1, [32]byte{}, &HubStatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: 48 * time.Hour}, 30*time.Second, completed)
	status, _ = retries.Status(1)
	require.Equal(t, completed.Add(maxHonoredRetryAfter), status.NextAttemptAt)
}

func TestSyncRetryClassification(t *testing.T) {
	notEnrolled := &HubStatusError{StatusCode: http.StatusForbidden, Body: `{"error":{"code":"` + db.FederationProjectNotEnrolledCode + `"}}`}
	for _, tc := range []struct {
		name   string
		err    error
		reason SyncRetryReason
		status int
	}{
		{"success", nil, "", 0},
		{"local failure", errors.New("disk full"), "", 0},
		{"bad gateway", &HubStatusError{StatusCode: http.StatusBadGateway}, "", 0},
		{"server error", &HubStatusError{StatusCode: http.StatusInternalServerError}, "", 0},
		{"forbidden", refusedSyncError(http.StatusForbidden), SyncRetryRefused, http.StatusForbidden},
		{"unauthorized", refusedSyncError(http.StatusUnauthorized), SyncRetryRefused, http.StatusUnauthorized},
		// The hub refused one pushed issue; the pull still runs and the
		// credential is still accepted.
		{"project not enrolled", notEnrolled, "", 0},
		{"refused pull after a refused push", errors.Join(notEnrolled, refusedSyncError(http.StatusForbidden)), SyncRetryRefused, http.StatusForbidden},
		{"throttled", &HubStatusError{StatusCode: http.StatusTooManyRequests}, SyncRetryUnavailable, http.StatusTooManyRequests},
		{"unavailable", fmt.Errorf("poll: %w", &HubStatusError{StatusCode: http.StatusServiceUnavailable}), SyncRetryUnavailable, http.StatusServiceUnavailable},
		{"refusal wins over unavailability", errors.Join(&HubStatusError{StatusCode: http.StatusServiceUnavailable}, refusedSyncError(http.StatusForbidden)), SyncRetryRefused, http.StatusForbidden},
		{"relay cross-project links", &HubStatusError{StatusCode: http.StatusConflict, relayCrossProjectLinks: true}, SyncRetryRelayCrossProjectLinks, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, status, _ := classifySyncRetry(tc.err)
			require.Equal(t, tc.reason, reason)
			require.Equal(t, tc.status, status)
		})
	}
}

func TestSyncRetryResets(t *testing.T) {
	binding := db.FederationBinding{ProjectID: 1, HubURL: "https://hub.example", HubProjectID: 42, Enabled: true}
	credential := config.FederationCredential{HubURL: "https://hub.example", HubProjectID: 42, Token: "token"}
	identity := mustSyncRetryIdentity(t, binding, credential)
	completed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	syncedAt := completed.Add(time.Second)
	for _, tc := range []struct {
		name    string
		changed db.FederationBinding
		cred    config.FederationCredential
	}{
		{"new token", binding, config.FederationCredential{HubURL: credential.HubURL, HubProjectID: 42, Token: "replacement"}},
		{"rebind to another hub URL", db.FederationBinding{ProjectID: 1, HubURL: "https://hub2.example", HubProjectID: 42, Enabled: true}, credential},
		{"another hub project", db.FederationBinding{ProjectID: 1, HubURL: binding.HubURL, HubProjectID: 43, Enabled: true}, credential},
		{"a sync succeeded elsewhere", db.FederationBinding{ProjectID: 1, HubURL: binding.HubURL, HubProjectID: 42, Enabled: true, LastSyncAt: &syncedAt}, credential},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retries := newSyncRetries(noJitter)
			retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
			require.True(t, retries.blocked(1, identity, completed))
			require.False(t, retries.blocked(1, mustSyncRetryIdentity(t, tc.changed, tc.cred), completed))
			_, ok := retries.Status(1)
			require.False(t, ok, "the changed binding starts a fresh schedule")
		})
	}
	t.Run("success", func(t *testing.T) {
		retries := newSyncRetries(noJitter)
		retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		retries.record(1, identity, nil, 30*time.Second, completed.Add(time.Minute))
		_, ok := retries.Status(1)
		require.False(t, ok)
		retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed.Add(time.Minute))
		status, _ := retries.Status(1)
		require.Equal(t, 1, status.Failures)
		require.Equal(t, completed.Add(2*time.Minute), status.NextAttemptAt)
	})
	t.Run("removed binding", func(t *testing.T) {
		retries := newSyncRetries(noJitter)
		retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		retries.record(2, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		retries.retain(map[int64]bool{2: true})
		_, ok := retries.Status(1)
		require.False(t, ok)
		_, ok = retries.Status(2)
		require.True(t, ok)
	})
	t.Run("runner restart", func(t *testing.T) {
		retries := newSyncRetries(noJitter)
		retries.record(1, identity, refusedSyncError(http.StatusForbidden), 30*time.Second, completed)
		retries.reset()
		require.False(t, retries.blocked(1, identity, completed))
	})
}

func TestSyncRetryNilIsInert(t *testing.T) {
	var retries *SyncRetries
	retries.record(1, [32]byte{}, refusedSyncError(http.StatusForbidden), 30*time.Second, time.Now())
	require.False(t, retries.blocked(1, [32]byte{}, time.Now()))
	_, ok := retries.Status(1)
	require.False(t, ok)
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "120", 2 * time.Minute},
		{"date", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"past date", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"absent", "", 0},
		{"malformed", "soon", 0},
		{"negative", "-5", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, parseRetryAfter(tc.header, now))
		})
	}
}

func TestHubStatusErrorCarriesRetryAfter(t *testing.T) {
	client := &Client{baseURL: "https://hub.example", client: &http.Client{Transport: pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("Retry-After", "120")
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: header, Body: io.NopCloser(strings.NewReader("busy")), Request: req}, nil
	})}}
	_, err := client.PollProjectEvents(t.Context(), 42, 0, 1)
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, 2*time.Minute, status.RetryAfter)
}

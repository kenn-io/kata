package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/linearsync"
	"go.kenn.io/kata/internal/notionsync"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/tickticksync"
	"go.kenn.io/kata/internal/todoistsync"
	"go.kenn.io/kata/internal/twentysync"
)

func TestOutcomesSyncTransitions(t *testing.T) {
	t.Run("retry failed enqueue", func(t *testing.T) {
		reporter := &fakeTelemetryReporter{failNext: true}
		observe := NewSyncOutcomeObserver(reporter)
		observe("github", 1, errors.New("failed"))
		require.Empty(t, reporter.captured)
		observe("github", 1, errors.New("still failing"))
		observe("github", 1, errors.New("still failing"))
		require.Equal(t, []capturedTelemetryEvent{
			{event: "sync_failed", properties: map[string]any{"sync_source": "github", "reason": "other"}},
		}, reporter.captured)
	})
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
		{&issuesync.StatusError{HTTPStatus: 429}, "rate_limit"},
		{&issuesync.StatusError{AuthFailure: true}, "auth"},
		{&issuesync.StatusError{RetryAfter: time.Minute}, "other"},
		{&issuesync.StatusError{HTTPStatus: 403, RateLimited: true}, "rate_limit"},
		{&issuesync.StatusError{HTTPStatus: 503, RetryAfter: time.Minute}, "server"},
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

func TestOutcomesLinearSemanticReasons(t *testing.T) {
	for _, tc := range []struct {
		provider, code, reason string
	}{
		{"linear", "FORBIDDEN", "auth"},
		{"linear", "AUTHENTICATION_ERROR", "auth"},
		{"linear", "AUTHENTICATION_REQUIRED", "auth"},
		{"linear", "UNAUTHENTICATED", "auth"},
		{"linear", "UNAUTHORIZED", "auth"},
		{"linear", "RATELIMITED", "rate_limit"},
		{"linear", "UNKNOWN", "other"},
		{"twenty", "UNAUTHENTICATED", "auth"},
		{"twenty", "FORBIDDEN", "auth"},
		{"twenty", "RATE_LIMITED", "rate_limit"},
		{"twenty", "UNKNOWN", "other"},
	} {
		t.Run(tc.provider+"/"+tc.code, func(t *testing.T) {
			now := time.Now()
			lookup := func(string) (string, bool) { return "example-token", true }
			transport := outcomeTransport(func(r *http.Request) (*http.Response, error) {
				if tc.provider == "twenty" {
					require.Equal(t, "/metadata", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"errors":[{"extensions":{"code":"` + tc.code + `"}}]}`))}, nil
			})
			var err error
			if tc.provider == "linear" {
				cfg := linearsync.Config{WorkspaceID: "11111111-1111-4111-8111-111111111111", TeamID: "22222222-2222-4222-8222-222222222222"}
				session, setupErr := linearsync.NewClient(linearsync.ClientConfig{
					LookupEnv: lookup, Transport: transport,
					Now:  func() time.Time { return now },
					Wait: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
				}).ForRun(t.Context(), cfg)
				require.NoError(t, setupErr)
				_, err = session.Scope(t.Context(), cfg)
			} else {
				cfg := twentysync.Config{APIOrigin: "https://twenty.example", WebOrigin: "https://twenty.example", WorkspaceID: "11111111-1111-4111-8111-111111111111"}
				session, setupErr := twentysync.NewClient(twentysync.ClientConfig{Daemon: config.TwentySyncConfig{APIOrigin: cfg.APIOrigin}, LookupEnv: lookup, Transport: transport}).ForRun(t.Context(), cfg)
				require.NoError(t, setupErr)
				_, err = session.Workspace(t.Context(), cfg)
			}
			require.Error(t, err)
			require.Equal(t, tc.reason, syncFailureReason(err))
		})
	}
}

type outcomeTransport func(*http.Request) (*http.Response, error)

func (rt outcomeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func TestOutcomesProviderTransportReasons(t *testing.T) {
	lookup := func(string) (string, bool) { return "example-token", true }
	const id = "11111111-1111-4111-8111-111111111111"
	for _, provider := range []struct {
		name string
		read func(*testing.T, http.RoundTripper) error
	}{
		{"linear", func(t *testing.T, rt http.RoundTripper) error {
			cfg := linearsync.Config{WorkspaceID: id, TeamID: id}
			session, err := linearsync.NewClient(linearsync.ClientConfig{LookupEnv: lookup, Transport: rt}).ForRun(t.Context(), cfg)
			require.NoError(t, err)
			_, err = session.Scope(t.Context(), cfg)
			return err
		}},
		{"plane", func(t *testing.T, rt http.RoundTripper) error {
			cfg := planesync.Config{APIOrigin: "https://api.plane.so", WebOrigin: "https://app.plane.so", Workspace: "example-workspace", ProjectID: id}
			session, err := planesync.NewClient(planesync.ClientConfig{LookupEnv: lookup, Transport: rt}).ForRun(t.Context(), cfg)
			require.NoError(t, err)
			_, err = session.Project(t.Context(), cfg)
			return err
		}},
		{"twenty", func(t *testing.T, rt http.RoundTripper) error {
			cfg := twentysync.Config{APIOrigin: "https://twenty.example", WebOrigin: "https://twenty.example", WorkspaceID: id}
			session, err := twentysync.NewClient(twentysync.ClientConfig{Daemon: config.TwentySyncConfig{APIOrigin: cfg.APIOrigin}, LookupEnv: lookup, Transport: rt}).ForRun(t.Context(), cfg)
			require.NoError(t, err)
			_, err = session.Workspace(t.Context(), cfg)
			return err
		}},
		{"notion", func(t *testing.T, rt http.RoundTripper) error {
			return readOutcomeNotionDatabase(t, rt)
		}},
		{"ticktick", func(t *testing.T, rt http.RoundTripper) error {
			session, err := tickticksync.NewClient(tickticksync.ClientConfig{LookupEnv: lookup, Transport: rt}).ForRun(t.Context(), tickticksync.Config{ProjectID: "project-1"})
			require.NoError(t, err)
			_, err = session.Project(t.Context())
			return err
		}},
		{"todoist", func(t *testing.T, rt http.RoundTripper) error {
			_, err := todoistsync.NewClient(todoistsync.ClientConfig{LookupEnv: lookup, Transport: rt}).Account(t.Context())
			return err
		}},
	} {
		for _, failure := range []struct {
			name   string
			err    error
			reason string
			body   bool
		}{
			{"network", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("example transport diagnostic")}, "network", false},
			{"deadline", context.DeadlineExceeded, "timeout", false},
			{"network timeout", &net.DNSError{IsTimeout: true}, "timeout", false},
			{"cancelled", context.Canceled, "", false},
			{"body network", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("example transport diagnostic")}, "network", true},
			{"body deadline", context.DeadlineExceeded, "timeout", true},
		} {
			t.Run(provider.name+"/"+failure.name, func(t *testing.T) {
				err := provider.read(t, outcomeTransport(func(*http.Request) (*http.Response, error) {
					if failure.body {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(iotest.ErrReader(failure.err))}, nil
					}
					return nil, failure.err
				}))
				require.Error(t, err)
				require.NotContains(t, err.Error(), "example transport diagnostic")
				if failure.reason == "" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.Equal(t, failure.reason, syncFailureReason(err))
				}
			})
		}
	}
}

func TestOutcomesNotionHTTPReasons(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
	}{{429, "rate_limit"}, {503, "server"}} {
		t.Run(tc.reason, func(t *testing.T) {
			requests := 0
			err := readOutcomeNotionDatabase(t, outcomeTransport(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"service_unavailable"}`))}, nil
			}))
			require.Error(t, err)
			require.Equal(t, 5, requests)
			reporter := &fakeTelemetryReporter{}
			NewSyncOutcomeObserver(reporter)("notion", 1, err)
			require.Equal(t, []capturedTelemetryEvent{{event: "sync_failed", properties: map[string]any{"sync_source": "notion", "reason": tc.reason}}}, reporter.captured)
		})
	}
}

func readOutcomeNotionDatabase(t *testing.T, rt http.RoundTripper) error {
	t.Helper()
	now := time.Now()
	session, err := notionsync.NewClient(notionsync.ClientConfig{
		LookupEnv: func(string) (string, bool) { return "example-token", true }, Transport: rt,
		Now:  func() time.Time { return now },
		Wait: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
	}).ForRun(t.Context())
	require.NoError(t, err)
	_, err = session.Database(t.Context(), "11111111-1111-4111-8111-111111111111")
	return err
}

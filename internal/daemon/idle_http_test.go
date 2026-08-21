package daemon

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

type rejectingIdleAdmission struct {
	calls atomic.Int32
}

func (a *rejectingIdleAdmission) TryForeground() (*IdleLease, bool) {
	a.calls.Add(1)
	return nil, false
}

func TestIdleHTTPAdmissionClassifiesRequests(t *testing.T) {
	t.Parallel()

	clock := newIdleTestClock(time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC))
	controller := newIdleControllerWithClock(time.Minute, nil, clock)
	controller.Start()
	initialDeadline := controller.Snapshot().Deadline

	var states []IdleState
	handler := withIdleAdmission(controller, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		states = append(states, controller.Snapshot().State)
		w.WriteHeader(http.StatusNoContent)
	}))

	clock.Advance(10 * time.Second)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	require.Equal(t, []IdleState{IdleStateArmed}, states)
	require.Equal(t, initialDeadline, controller.Snapshot().Deadline)

	clock.Advance(10 * time.Second)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	request.Header.Set(IdleKeepaliveHeader, "1")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.Equal(t, []IdleState{IdleStateArmed, IdleStateForeground}, states)
	require.Equal(t, clock.Now().Add(time.Minute), controller.Snapshot().Deadline)

	clock.Advance(10 * time.Second)
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	require.Equal(t, []IdleState{IdleStateArmed, IdleStateForeground, IdleStateForeground}, states)
	require.Equal(t, clock.Now().Add(time.Minute), controller.Snapshot().Deadline)
}

func TestIdleHTTPAdmissionRejectsRequestsAfterStopping(t *testing.T) {
	t.Parallel()

	controller := NewIdleController(time.Minute, nil)
	controller.Start()
	controller.Stop()
	called := false
	handler := withIdleAdmission(controller, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.False(t, called)
	require.Contains(t, recorder.Body.String(), `"code":"daemon_stopping"`)
}

func TestIdleHTTPAdmissionLeavesObservationalProbesUnleased(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/api/v1/ping", "/api/v1/health", "/api/v1/instance"} {
		t.Run(path, func(t *testing.T) {
			controller := NewIdleController(time.Minute, nil)
			controller.Start()
			controller.Stop()
			called := false
			handler := withIdleAdmission(controller, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}))

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

			require.Equal(t, http.StatusNoContent, recorder.Code)
			require.True(t, called)
		})
	}
}

func TestIdleKeepaliveMarkerOnlyUpgradesGetPing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{name: "get ping", method: http.MethodGet, path: "/api/v1/ping", want: true},
		{name: "post ping", method: http.MethodPost, path: "/api/v1/ping"},
		{name: "get health", method: http.MethodGet, path: "/api/v1/health"},
		{name: "get instance", method: http.MethodGet, path: "/api/v1/instance"},
		{name: "get data", method: http.MethodGet, path: "/api/v1/projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			request.Header.Set(IdleKeepaliveHeader, "1")
			require.Equal(t, tc.want, isMarkedIdleKeepalive(request))
		})
	}
}

func TestServerIdleAdmissionRunsAfterRequestValidation(t *testing.T) {
	t.Parallel()

	newServer := func(t *testing.T, admission IdleForegroundAdmission) *Server {
		t.Helper()
		return NewServer(ServerConfig{
			Auth:          config.AuthConfig{Token: "expected-token"},
			IdleAdmission: admission,
		})
	}

	t.Run("missing bearer", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		recorder := httptest.NewRecorder()
		newServer(t, admission).Handler().ServeHTTP(
			recorder, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))

		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("cross origin socket request", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.Header.Set("Origin", "https://attacker.example")
		recorder := httptest.NewRecorder()
		newServer(t, admission).Handler().ServeHTTP(recorder, request)

		require.Equal(t, http.StatusForbidden, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("cross origin preflight", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		request := httptest.NewRequest(http.MethodOptions, "/api/v1/projects", nil)
		request.Header.Set("Origin", "https://attacker.example")
		request.Header.Set("Access-Control-Request-Method", http.MethodPost)
		recorder := httptest.NewRecorder()
		newServer(t, admission).Handler().ServeHTTP(recorder, request)

		require.Equal(t, http.StatusForbidden, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("browser preflight reaching the route stack", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		server := newServer(t, admission)
		handler, err := server.HandlerFor(ListenerPolicy{
			Kind:   ListenerBrowser,
			Origin: "https://kata.example",
		})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodOptions, "https://kata.example/api/v1/ping", nil)
		request.Host = "kata.example"
		request.Header.Set("Origin", "https://attacker.example")
		request.Header.Set("Access-Control-Request-Method", http.MethodGet)
		request.Header.Set("Access-Control-Request-Headers", IdleKeepaliveHeader)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		require.NotEqual(t, http.StatusServiceUnavailable, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("invalid browser host", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		server := newServer(t, admission)
		handler, err := server.HandlerFor(ListenerPolicy{
			Kind:   ListenerBrowser,
			Origin: "https://kata.example",
		})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.Host = "attacker.example"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("missing browser session", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		manager := newDeterministicSessionManager(t, "https://kata.example", "idle_test")
		server := NewServer(ServerConfig{WebSessions: manager, IdleAdmission: admission})
		handler, err := server.HandlerFor(ListenerPolicy{
			Kind:                  ListenerBrowser,
			Origin:                "https://kata.example",
			RequireBrowserSession: true,
		})
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodGet, "https://kata.example/api/v1/projects", nil)
		request.Host = "kata.example"
		request.Header.Set("Origin", "https://kata.example")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		require.Zero(t, admission.calls.Load())
	})

	t.Run("validated bearer request", func(t *testing.T) {
		admission := &rejectingIdleAdmission{}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
		request.Header.Set("Authorization", "Bearer expected-token")
		recorder := httptest.NewRecorder()
		newServer(t, admission).Handler().ServeHTTP(recorder, request)

		require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
		require.Equal(t, int32(1), admission.calls.Load())
	})
}

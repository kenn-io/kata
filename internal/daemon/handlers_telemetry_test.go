package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/telemetry"
)

const telemetryTestOrigin = "http://127.0.0.1:27123"

type telemetryTestServer struct {
	handler http.Handler
	session IssuedWebSession
	manager *WebSessionManager
}

func newTelemetryTestServer(t *testing.T, reporter TelemetryReporter, principal Principal) telemetryTestServer {
	t.Helper()
	store := openAuthTestDB(t)
	manager := newDeterministicSessionManager(t, telemetryTestOrigin, "instance_a")
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager, Telemetry: reporter,
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerBrowser, Origin: telemetryTestOrigin,
		RequireBrowserSession: true, AllowLocalSession: true,
	})
	require.NoError(t, err)
	issued, err := manager.IssueSession(principal, "/kata")
	require.NoError(t, err)
	return telemetryTestServer{handler: handler, session: issued, manager: manager}
}

// newDisabledReporter is the go-test reporter: disabled, with kata's allowlist.
func newDisabledReporter(t *testing.T) TelemetryReporter {
	t.Helper()
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	return reporter
}

type fakeTelemetryReporter struct {
	failNext bool
	captured []map[string]any
}

func (*fakeTelemetryReporter) EventAllowed(event string) bool {
	return strings.TrimSpace(event) == "app_opened" || strings.TrimSpace(event) == "session_ended"
}
func (*fakeTelemetryReporter) Enabled() bool { return true }
func (f *fakeTelemetryReporter) Capture(_ string, properties map[string]any) error {
	if f.failNext {
		f.failNext = false
		return errors.New("capture failed")
	}
	f.captured = append(f.captured, properties)
	return nil
}

type telemetryRequestOption func(*http.Request)

func withoutSessionHeaders(r *http.Request) {
	r.Header.Del(webSessionHeader)
	r.Header.Del(webCSRFHeader)
}

func withoutCSRF(r *http.Request) { r.Header.Del(webCSRFHeader) }

func withHeader(name, value string) telemetryRequestOption {
	return func(r *http.Request) { r.Header.Set(name, value) }
}

func (s telemetryTestServer) post(ctx context.Context, t *testing.T, body string, options ...telemetryRequestOption) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, telemetryTestOrigin+"/api/v1/ui/telemetry", strings.NewReader(body))
	request.Host = "127.0.0.1:27123"
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Origin", telemetryTestOrigin)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(s.manager.Cookie(s.session.Cookie))
	request.Header.Set(webSessionHeader, s.session.Session)
	request.Header.Set(webCSRFHeader, s.session.CSRF)
	for _, option := range options {
		option(request)
	}
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	return response
}

func decodeErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope), response.Body.String())
	return envelope.Error.Code
}

func TestCaptureTelemetryEventAcceptsAppOpened(t *testing.T) {
	server := newTelemetryTestServer(t, newDisabledReporter(t), Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.JSONEq(t, `{"status":"disabled"}`, response.Body.String())
}

func TestCaptureTelemetryEventRejectsUnknownEventWithEnvelope(t *testing.T) {
	server := newTelemetryTestServer(t, newDisabledReporter(t), Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_loaded"}`)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, "unsupported_telemetry_event", decodeErrorCode(t, response))
}

func TestCaptureTelemetryEventRejectsOversizedBody(t *testing.T) {
	server := newTelemetryTestServer(t, newDisabledReporter(t), Principal{Kind: PrincipalWebLocal})
	body := `{"event":"app_opened","properties":{"padding":"` + strings.Repeat("x", 16<<10) + `"}}`

	response := server.post(t.Context(), t, body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code, response.Body.String())
}

func TestCaptureTelemetryEventRequiresBrowserGuards(t *testing.T) {
	for _, test := range []struct {
		name      string
		principal Principal
		option    telemetryRequestOption
		want      int
	}{
		{name: "no session headers", principal: Principal{Kind: PrincipalWebLocal}, option: withoutSessionHeaders, want: http.StatusUnauthorized},
		{name: "no csrf", principal: Principal{Kind: PrincipalWebLocal}, option: withoutCSRF, want: http.StatusForbidden},
		{name: "origin on another port", principal: Principal{Kind: PrincipalWebLocal}, option: withHeader("Origin", "http://127.0.0.1:27124"), want: http.StatusForbidden},
		{name: "plain text body", principal: Principal{Kind: PrincipalWebLocal}, option: withHeader("Content-Type", "text/plain"), want: http.StatusUnsupportedMediaType},
		{name: "read-only principal", principal: Principal{Kind: PrincipalBootstrap}, option: func(*http.Request) {}, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := &fakeTelemetryReporter{}
			server := newTelemetryTestServer(t, reporter, test.principal)

			response := server.post(t.Context(), t, `{"event":"app_opened"}`, test.option)

			assert.Equal(t, test.want, response.Code, response.Body.String())
			assert.Empty(t, reporter.captured, "guards must reject before capture")
		})
	}
}

func TestCaptureTelemetryEventUnavailableWithoutCaptureHandler(t *testing.T) {
	server := newTelemetryTestServer(t, nil, Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "telemetry_unavailable", decodeErrorCode(t, response))
}

func TestCaptureTelemetryEventQueuesThroughEnabledReporter(t *testing.T) {
	reporter := &fakeTelemetryReporter{}
	server := newTelemetryTestServer(t, reporter, Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened","properties":{"surface":"web"}}`)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	assert.Equal(t, []map[string]any{{"surface": "web"}}, reporter.captured)
	for range 2 {
		response := server.post(t.Context(), t, `{"event":"session_ended","properties":{"surface":"web","duration_bucket":"1_to_5m"}}`)
		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	}
	assert.Equal(t, []map[string]any{{"surface": "web", "duration_bucket": "1_to_5m"}, {"surface": "web", "duration_bucket": "1_to_5m"}}, reporter.captured[1:])
}

// A TUI posts with a bearer token and no browser markers; both listener kinds must accept it.
func TestCaptureTelemetryAcceptsBearerClientOnEitherListener(t *testing.T) {
	const host = "daemon.example"
	for _, kind := range []ListenerKind{ListenerSharedTCP, ListenerBrowser} {
		for _, test := range []struct {
			name  string
			auth  config.AuthConfig
			token string
		}{
			{name: "configured token", auth: config.AuthConfig{Token: "configured-token"}, token: "configured-token"},
			{name: "identity token", auth: config.AuthConfig{Token: "configured-token", RequireTokenIdentity: true}, token: "user-token"},
		} {
			t.Run(string(kind)+"/"+test.name, func(t *testing.T) {
				store := openAuthTestDB(t)
				_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
					PlaintextToken: "user-token", Actor: "alice", AdminActor: db.BootstrapActor,
				})
				require.NoError(t, err)
				manager, err := NewWebSessionManager(WebSessionManagerConfig{
					Origin: "https://" + host, InstanceID: "instance_a", Writable: true, Auth: test.auth, DB: store,
				})
				require.NoError(t, err)
				reporter := &fakeTelemetryReporter{}
				server := NewServer(ServerConfig{
					DB: store, StartedAt: time.Now().UTC(), WebSessions: manager, Auth: test.auth, Telemetry: reporter,
				})
				t.Cleanup(func() { _ = server.Close() })
				handler, err := server.HandlerFor(ListenerPolicy{
					Kind: kind, Origin: "https://" + host, BackendAuthority: "127.0.0.1:7777", RequireBrowserSession: true,
				})
				require.NoError(t, err)
				request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+host+"/api/v1/ui/telemetry",
					strings.NewReader(`{"event":"app_opened","properties":{"surface":"tui"}}`))
				request.RemoteAddr = "127.0.0.1:40123"
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+test.token)
				response := httptest.NewRecorder()

				handler.ServeHTTP(response, request)

				require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
				assert.Equal(t, []map[string]any{{"surface": "tui"}}, reporter.captured)
			})
		}
	}
}

func TestAppOpenedGate(t *testing.T) {
	var calls int
	forward := func(captured bool) func() bool {
		return func() bool {
			calls++
			return captured
		}
	}

	t.Run("one capture per surface per UTC day", func(t *testing.T) {
		calls = 0
		now := time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)
		gate := newAppOpenedGate(func() time.Time { return now })

		gate.capture("cli", forward(true))
		gate.capture("cli", forward(true))
		gate.capture("tui", forward(true))
		gate.capture("", forward(true))
		gate.capture("", forward(true))
		assert.Equal(t, 3, calls, "each surface, the empty one included, captures once a day")

		now = now.Add(2 * time.Hour)
		gate.capture("cli", forward(true))
		assert.Equal(t, 4, calls, "a new UTC day captures again")
	})

	t.Run("a failed capture leaves the surface unmarked", func(t *testing.T) {
		calls = 0
		gate := newAppOpenedGate(time.Now)

		gate.capture("cli", forward(false))
		gate.capture("cli", forward(true))
		gate.capture("cli", forward(true))
		assert.Equal(t, 2, calls)
	})

	t.Run("concurrent first calls wait for the first outcome", func(t *testing.T) {
		gate := newAppOpenedGate(time.Now)
		var forwards atomic.Int32
		inForward := make(chan struct{})
		release := make(chan struct{})
		blocking := func() bool {
			if forwards.Add(1) == 1 {
				close(inForward)
				<-release
			}
			return true
		}
		done := make(chan struct{}, 2)
		go func() { gate.capture("cli", blocking); done <- struct{}{} }()
		<-inForward
		go func() { gate.capture("cli", blocking); done <- struct{}{} }()

		select {
		case <-done:
			t.Fatal("a caller returned while the first capture was still pending")
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		<-done
		<-done
		assert.Equal(t, int32(1), forwards.Load())
	})
}

func TestCaptureTelemetryEventCapturesOneAppOpenedPerSurfacePerDay(t *testing.T) {
	reporter := &fakeTelemetryReporter{failNext: true}
	server := newTelemetryTestServer(t, reporter, Principal{Kind: PrincipalWebLocal})
	tui := `{"event":"app_opened","properties":{"surface":"tui"}}`

	failed := server.post(t.Context(), t, tui)
	require.Equal(t, http.StatusInternalServerError, failed.Code, failed.Body.String())
	for _, body := range []string{
		tui, tui,
		`{"event":"app_opened","properties":{"surface":"web"}}`,
		`{"event":"app_opened","properties":{"surface":" tui "}}`,
		`{"event":" app_opened ","properties":{"surface":"tui"}}`,
	} {
		response := server.post(t.Context(), t, body)
		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
		assert.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	}
	assert.Equal(t, []map[string]any{{"surface": "tui"}, {"surface": "web"}}, reporter.captured,
		"a failed capture leaves the day open; later duplicates are dropped")
}

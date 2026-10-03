package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func newTelemetryTestServer(t *testing.T, capture http.Handler, principal Principal) telemetryTestServer {
	t.Helper()
	store := openAuthTestDB(t)
	manager := newDeterministicSessionManager(t, telemetryTestOrigin, "instance_a")
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager, TelemetryCapture: capture,
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

func newReporterCaptureHandler(t *testing.T) http.Handler {
	t.Helper()
	reporter, err := telemetry.NewReporter(telemetry.Options{})
	require.NoError(t, err)
	return telemetry.CaptureHandler(reporter)
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
	server := newTelemetryTestServer(t, newReporterCaptureHandler(t), Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.JSONEq(t, `{"status":"disabled"}`, response.Body.String())
}

func TestCaptureTelemetryEventRejectsUnknownEventWithEnvelope(t *testing.T) {
	server := newTelemetryTestServer(t, newReporterCaptureHandler(t), Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_loaded"}`)

	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.Equal(t, "unsupported_telemetry_event", decodeErrorCode(t, response))
}

func TestCaptureTelemetryEventRejectsOversizedBody(t *testing.T) {
	server := newTelemetryTestServer(t, newReporterCaptureHandler(t), Principal{Kind: PrincipalWebLocal})
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
			captured := false
			capture := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				captured = true
				w.WriteHeader(http.StatusAccepted)
			})
			server := newTelemetryTestServer(t, capture, test.principal)

			response := server.post(t.Context(), t, `{"event":"app_opened"}`, test.option)

			assert.Equal(t, test.want, response.Code, response.Body.String())
			assert.False(t, captured, "guards must reject before capture")
		})
	}
}

func TestCaptureTelemetryEventUnavailableWithoutCaptureHandler(t *testing.T) {
	server := newTelemetryTestServer(t, nil, Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Equal(t, "telemetry_unavailable", decodeErrorCode(t, response))
}

type telemetryContextKey struct{}

func TestCaptureTelemetryEventForwardsBodyAndContextToCaptureHandler(t *testing.T) {
	var gotBody []byte
	var gotContextValue any
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotContextValue = r.Context().Value(telemetryContextKey{})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"queued"}`))
	})
	server := newTelemetryTestServer(t, capture, Principal{Kind: PrincipalWebLocal})
	ctx := context.WithValue(t.Context(), telemetryContextKey{}, "caller")

	response := server.post(ctx, t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
	assert.JSONEq(t, `{"status":"queued"}`, response.Body.String())
	assert.JSONEq(t, `{"event":"app_opened"}`, string(bytes.TrimSpace(gotBody)))
	assert.Equal(t, "caller", gotContextValue)
}

func TestCaptureTelemetryEventMapsCaptureFailure(t *testing.T) {
	capture := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := newTelemetryTestServer(t, capture, Principal{Kind: PrincipalWebLocal})

	response := server.post(t.Context(), t, `{"event":"app_opened"}`)

	require.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	assert.Equal(t, "telemetry_capture_failed", decodeErrorCode(t, response))
}

const tuiTelemetryBody = `{"event":"app_opened","properties":{"surface":"tui"}}`

type recordingTelemetryCapture struct{ bodies []string }

func (c *recordingTelemetryCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.bodies = append(c.bodies, string(bytes.TrimSpace(body)))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"queued"}`))
}

func newSharedTCPTelemetryHandler(t *testing.T, auth config.AuthConfig, origin string, capture http.Handler, identityTokens ...string) http.Handler {
	t.Helper()
	store := openAuthTestDB(t)
	for _, token := range identityTokens {
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
			PlaintextToken: token, Actor: "alice", AdminActor: db.BootstrapActor,
		})
		require.NoError(t, err)
	}
	manager, err := NewWebSessionManager(WebSessionManagerConfig{
		Origin: origin, InstanceID: "instance_a", Writable: true, Auth: auth, DB: store,
	})
	require.NoError(t, err)
	server := NewServer(ServerConfig{
		DB: store, StartedAt: time.Now().UTC(), WebSessions: manager, Auth: auth, TelemetryCapture: capture,
	})
	t.Cleanup(func() { _ = server.Close() })
	handler, err := server.HandlerFor(ListenerPolicy{
		Kind: ListenerSharedTCP, Origin: origin, BackendAuthority: "127.0.0.1:7777", RequireBrowserSession: true,
	})
	require.NoError(t, err)
	return handler
}

func postSharedTCPTelemetry(t *testing.T, handler http.Handler, host string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+host+"/api/v1/ui/telemetry", strings.NewReader(tuiTelemetryBody))
	request.Host = host
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestCaptureTelemetrySharedTCPBearerClient(t *testing.T) {
	for _, test := range []struct {
		name     string
		headers  map[string]string
		want     int
		wantCode string
		captured bool
	}{
		{name: "valid token", headers: map[string]string{"Authorization": "Bearer configured-token"}, want: http.StatusAccepted, captured: true},
		{name: "wrong token", headers: map[string]string{"Authorization": "Bearer wrong-token"}, want: http.StatusForbidden},
		{
			name:    "valid token with foreign origin",
			headers: map[string]string{"Authorization": "Bearer configured-token", "Origin": "https://evil.example"},
			want:    http.StatusForbidden, wantCode: "origin_forbidden",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := &recordingTelemetryCapture{}
			handler := newSharedTCPTelemetryHandler(t, config.AuthConfig{Token: "configured-token"}, "https://daemon.example", capture)

			response := postSharedTCPTelemetry(t, handler, "daemon.example", test.headers)

			require.Equal(t, test.want, response.Code, response.Body.String())
			if test.wantCode != "" {
				assert.Equal(t, test.wantCode, decodeErrorCode(t, response))
			}
			if test.captured {
				assert.Equal(t, []string{tuiTelemetryBody}, capture.bodies)
			} else {
				assert.Empty(t, capture.bodies)
			}
		})
	}

	t.Run("keyless loopback client", func(t *testing.T) {
		capture := &recordingTelemetryCapture{}
		handler := newSharedTCPTelemetryHandler(t, config.AuthConfig{}, "http://127.0.0.1:7777", capture)

		response := postSharedTCPTelemetry(t, handler, "127.0.0.1:7777", nil)

		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
		assert.Equal(t, []string{tuiTelemetryBody}, capture.bodies)
	})
	// Identity mode can't start without a bootstrap token, so the no-token case is unreachable.
	t.Run("identity token", func(t *testing.T) {
		capture := &recordingTelemetryCapture{}
		auth := config.AuthConfig{Token: "configured-token", RequireTokenIdentity: true}
		handler := newSharedTCPTelemetryHandler(t, auth, "https://daemon.example", capture, "user-token")

		response := postSharedTCPTelemetry(t, handler, "daemon.example", map[string]string{"Authorization": "Bearer user-token"})

		require.Equal(t, http.StatusAccepted, response.Code, response.Body.String())
		assert.Equal(t, []string{tuiTelemetryBody}, capture.bodies)
	})
}

package mcpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"go.kenn.io/kata/internal/mcpevents"
	kataclient "go.kenn.io/kata/pkg/client"
)

const wantEventsExtensionID = "io.modelcontextprotocol/events"

func discover(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	recorder, wire := serve(handler, currentRequest("server/discover", "", eventsRPC("server/discover", "")))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	result, ok := wire["result"].(map[string]any)
	require.True(t, ok, recorder.Body.String())
	return result
}

func TestCurrentPathAdvertisesEvents(t *testing.T) {
	_, client := newRecordingDaemon(t)
	result := discover(t, newTestHTTPHandler(t, client, boundTestScope(t)))
	capabilities := result["capabilities"].(map[string]any)
	assert.Equal(t, map[string]any{}, capabilities["events"])
	extensions := capabilities["extensions"].(map[string]any)
	assert.Equal(t, map[string]any{}, extensions[wantEventsExtensionID])
	assert.NotContains(t, capabilities["events"], "listChanged")
}

func TestLegacySessionServerNeverAdvertisesOrServesEvents(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	handler := newTestHTTPHandler(t, client, boundTestScope(t))
	init := func(method, params string) *http.Request {
		request := currentRequest(method, "", `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`)
		request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		request.Header.Del("Mcp-Method")
		return request
	}
	recorder, wire := serve(handler, init("initialize", `{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}`))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	body := recorder.Body.String()
	if wire == nil {
		// A legacy session answers as server-sent events.
		assert.NotContains(t, body, "events")
	}
	assert.NotContains(t, body, wantEventsExtensionID)
	assert.NotContains(t, body, `"events"`)
	session := recorder.Header().Get("Mcp-Session-Id")
	require.NotEmpty(t, session)

	request := init("events/subscribe", `{`+eventsParams("")+`}`)
	request.Header.Set("Mcp-Session-Id", session)
	recorder, _ = serve(handler, request)
	// The legacy SDK answers an unregistered method as not handled (JSON-RPC
	// method not found) instead of reaching any Events code.
	assert.Contains(t, recorder.Body.String(), "events/subscribe", recorder.Body.String())
	assert.Contains(t, recorder.Body.String(), "not handled", recorder.Body.String())
	assert.NotEqual(t, http.StatusOK, recorder.Code)
	assert.Zero(t, daemon.eventCalls())
}

func TestResultsCarryCompleteResultType(t *testing.T) {
	_, client := newRecordingDaemon(t)
	handler := newTestHTTPHandler(t, client, boundTestScope(t))
	for _, tc := range []struct{ method, name, params string }{
		{"events/list", "", ""},
		{"events/subscribe", testEventName, eventsParams("")},
		{"events/unsubscribe", testEventName, eventsParams("")},
	} {
		t.Run(tc.method, func(t *testing.T) {
			recorder, wire := serve(handler, currentRequest(tc.method, tc.name, eventsRPC(tc.method, tc.params)))
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			result, ok := wire["result"].(map[string]any)
			require.True(t, ok, recorder.Body.String())
			assert.Equal(t, "complete", result["resultType"])
		})
	}
}

func TestSubscribeForwardsWireShapeAndScope(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	handler := newTestHTTPHandler(t, client, boundTestScope(t))
	recorder, wire := serve(handler, currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe",
		eventsParams(`"ttlMs":null,"maxAgeMs":5000,"cursor":"c1.x"`))))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	result := wire["result"].(map[string]any)
	assert.Equal(t, "sub_fixture", result["id"])
	assert.Contains(t, result, "refreshBefore")
	assert.Nil(t, result["refreshBefore"])
	assert.Equal(t, map[string]any{"active": true}, result["deliveryStatus"])
	calls := daemon.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "/api/v1/mcp-events/subscribe", calls[0].Path)
	assert.Equal(t, true, calls[0].Body["no_expiry"])
	assert.NotContains(t, calls[0].Body, "ttl_ms")
	assert.EqualValues(t, 5000, calls[0].Body["max_age_ms"])
	assert.Equal(t, "c1.x", calls[0].Body["cursor"])
	assert.Equal(t, map[string]any{"projects": []any{testProjectUID}}, calls[0].Body["forwarder_scope"])
	assert.Equal(t, map[string]any{"for": "example-agent/worker", "all_projects": true}, calls[0].Body["arguments"],
		"an omitted MCP selection asks the daemon for every project explicitly")
}

func TestForwarderScopeFollowsTheScopeMode(t *testing.T) {
	all := NewAllScope()
	allowlist, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 2, UID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Name: "other-project"},
		{ID: 1, UID: testProjectUID, Name: "spoke-project"},
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		desc  string
		scope *Scope
		want  map[string]any
	}{
		{"all", all, map[string]any{"all": true}},
		{"bound", boundTestScope(t), map[string]any{"projects": []any{testProjectUID}}},
		{"allowlist", allowlist, map[string]any{"projects": []any{"01ARZ3NDEKTSV4RRFFQ69G5FAB", testProjectUID}}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			daemon, client := newRecordingDaemon(t)
			options := Options{Client: client, Scope: tc.scope, Actor: "example-agent", Version: "test", Events: true}
			server, err := New(options)
			require.NoError(t, err)
			handler := NewHTTPHandler(server, server)
			recorder, _ := serve(handler, currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe", eventsParams(""))))
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			calls := daemon.calls()
			require.NotEmpty(t, calls)
			subscribe := calls[len(calls)-1]
			assert.Equal(t, "/api/v1/mcp-events/subscribe", subscribe.Path)
			assert.ElementsMatch(t, mapKeys(tc.want), mapKeys(subscribe.Body["forwarder_scope"].(map[string]any)))
			if projects, ok := tc.want["projects"]; ok {
				assert.ElementsMatch(t, projects, subscribe.Body["forwarder_scope"].(map[string]any)["projects"])
			} else {
				assert.Equal(t, true, subscribe.Body["forwarder_scope"].(map[string]any)["all"])
			}
		})
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func TestEmptyProjectUIDsAreRejectedBeforeTheDaemon(t *testing.T) {
	for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
		t.Run(method, func(t *testing.T) {
			daemon, client := newRecordingDaemon(t)
			handler := newTestHTTPHandler(t, client, boundTestScope(t))
			params := `"name":"kata.inbox.changed","arguments":{"for":"example-agent/worker","project_uids":[]},"delivery":{"mode":"webhook","url":"` + testCallback + `","secret":"` + testSecret + `"}`
			recorder, wire := serve(handler, currentRequest(method, testEventName, eventsRPC(method, params)))
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			errObj := wire["error"].(map[string]any)
			assert.InDelta(t, -32602.0, errObj["code"], 0.5)
			assert.Equal(t, "InvalidParams", errObj["message"])
			assert.Equal(t, map[string]any{"reason": "invalid_project_uids"}, errObj["data"])
			assert.Zero(t, daemon.eventCalls())
		})
	}
}

func TestMalformedParamsAreInvalidParams(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	handler := newTestHTTPHandler(t, client, boundTestScope(t))
	for _, params := range []string{
		eventsParams(`"surprise":1`),
		`"name":"kata.inbox.changed","arguments":"x","delivery":{}`,
		`"name":"kata.inbox.changed","arguments":{"for":"a","extra":1},"delivery":{"mode":"webhook","url":"u","secret":"s"}`,
	} {
		recorder, wire := serve(handler, currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe", params)))
		require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		errObj := wire["error"].(map[string]any)
		assert.InDelta(t, -32602.0, errObj["code"], 0.5, params)
		assert.Equal(t, "InvalidParams", errObj["message"])
	}
	assert.Zero(t, daemon.eventCalls())
}

func TestDaemonErrorsMapToTheWireTable(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		status int
		code   int
		data   string
		name   string
	}{
		{"not found", 404, mcpevents.CodeNotFound, `{"kind":"subscription"}`, "NotFound"},
		{"forbidden", 403, mcpevents.CodeForbidden, `{"reason":"scope"}`, "Forbidden"},
		{"exhausted", 429, mcpevents.CodeResourceExhausted, `{"limit":"subscriptions","max":128}`, "ResourceExhausted"},
		{"unsupported", 400, mcpevents.CodeUnsupported, `{"feature":"deliveryMode"}`, "Unsupported"},
		{"callback", 502, mcpevents.CodeCallbackEndpoint, `{"reason":"timeout"}`, "CallbackEndpointError"},
		{"invalid", 400, mcpevents.CodeInvalidParams, `{"reason":"invalid_secret"}`, "InvalidParams"},
		{"disabled", 503, mcpevents.CodeUnavailable, `{"reason":"events_not_configured"}`, "Unavailable"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			daemon, client := newRecordingDaemon(t)
			daemon.reply = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"status":` + itoa(tc.status) + `,"error":{"code":"mcp_events","message":"MCP Events: private detail https://secret.example","data":{"mcp_code":` + itoa(tc.code) + `,"mcp_data":` + tc.data + `}}}`))
			}
			handler := newTestHTTPHandler(t, client, boundTestScope(t))
			recorder, wire := serve(handler, currentRequest("events/unsubscribe", testEventName, eventsRPC("events/unsubscribe", eventsParams(""))))
			wantStatus := http.StatusOK
			if tc.code == mcpevents.CodeInvalidParams {
				wantStatus = http.StatusBadRequest // the SDK's HTTP mapping of -32602
			}
			require.Equal(t, wantStatus, recorder.Code, recorder.Body.String())
			errObj := wire["error"].(map[string]any)
			assert.InDelta(t, float64(tc.code), errObj["code"], 0.5)
			assert.Equal(t, tc.name, errObj["message"])
			wantData := map[string]any{}
			require.NoError(t, jsonUnmarshal(tc.data, &wantData))
			assert.Equal(t, wantData, errObj["data"])
			assert.NotContains(t, recorder.Body.String(), "secret.example")
		})
	}
	t.Run("daemon unreachable", func(t *testing.T) {
		daemon, client := newRecordingDaemon(t)
		daemon.reply = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, wire := serve(handler, currentRequest("events/list", "", eventsRPC("events/list", "")))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		errObj := wire["error"].(map[string]any)
		assert.InDelta(t, float64(mcpevents.CodeUnavailable), errObj["code"], 0.5)
		assert.Equal(t, "Unavailable", errObj["message"])
	})
}

func TestServerWithoutEventsAnswersMethodNotFound(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	server, err := New(Options{Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test"})
	require.NoError(t, err)
	handler := NewHTTPHandler(server, server)
	result := discover(t, handler)
	assert.NotContains(t, result["capabilities"], "events")
	assert.NotContains(t, result["capabilities"], "extensions")
	recorder, wire := serve(handler, currentRequest("events/list", "", eventsRPC("events/list", "")))
	errObj, ok := wire["error"].(map[string]any)
	require.True(t, ok, recorder.Body.String())
	assert.InDelta(t, -32601.0, errObj["code"], 0.5)
	assert.Zero(t, daemon.eventCalls())
}

func TestEventSubscriptionsToolReadsTheDaemonStatusRoute(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	daemon.reply = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"enabled":true,"subscriptions":[{"id":"sub_fixture","name":"kata.inbox.changed","recipient":"example-agent/worker","project_uids":["01ARZ3NDEKTSV4RRFFQ69G5FAA"],"state":"active","refresh_before":null,"cursor":"c1.x","continuity_generation":2,"gap_unacked":false,"delivery":{"active":true,"last_error":"timeout"},"failed_attempts":1,"dead_letter_count":0}]}`))
	}
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test", Events: true,
	})
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "kata.event_subscriptions")

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.event_subscriptions", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, result.IsError, mustJSON(t, result))
	output := result.StructuredContent.(map[string]any)
	assert.Equal(t, true, output["enabled"])
	subscriptions := output["subscriptions"].([]any)
	require.Len(t, subscriptions, 1)
	first := subscriptions[0].(map[string]any)
	assert.Equal(t, "sub_fixture", first["id"])
	assert.Equal(t, "example-agent/worker", first["recipient"])
	assert.Equal(t, map[string]any{"active": true, "lastError": "timeout"}, first["delivery"])
	calls := daemon.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "/api/v1/mcp-events/subscriptions", calls[0].Path)
	assert.Equal(t, http.MethodGet, calls[0].Method)
}

func TestEventToolsAbsentWithoutEvents(t *testing.T) {
	_, client := newRecordingDaemon(t)
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test",
	})
	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		assert.NotContains(t, tool.Name, "event_")
	}
}

func TestDaemonCodesOutsideTheWireTableBecomeUnavailable(t *testing.T) {
	for _, code := range []int{-32099, -32021, -32600, 5, -31999} {
		daemon, client := newRecordingDaemon(t)
		daemon.reply = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":502,"error":{"code":"x","message":"m","data":{"mcp_code":` + itoa(code) + `,"mcp_data":{"x":1}}}}`))
		}
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		_, wire := serve(handler, currentRequest("events/list", "", eventsRPC("events/list", "")))
		errObj := wire["error"].(map[string]any)
		assert.InDelta(t, float64(mcpevents.CodeUnavailable), errObj["code"], 0.5, code)
		assert.Equal(t, map[string]any{"reason": "daemon_unavailable"}, errObj["data"])
	}
}

func TestUnresolvableForwarderScopeIsAFixedUnavailable(t *testing.T) {
	daemon, client := newRecordingDaemon(t)
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, Name: "spoke-project"}) // no UID
	require.NoError(t, err)
	handler := newTestHTTPHandler(t, client, scope)
	_, wire := serve(handler, currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe", eventsParams(""))))
	errObj := wire["error"].(map[string]any)
	assert.InDelta(t, float64(mcpevents.CodeUnavailable), errObj["code"], 0.5)
	assert.Equal(t, map[string]any{"reason": "forwarder_scope_unavailable"}, errObj["data"])
	assert.Zero(t, daemon.eventCalls())
}

func TestEventSubscriptionsDisplayFilterFollowsTheForwarderScope(t *testing.T) {
	const other = "01ARZ3NDEKTSV4RRFFQ69G5FAB"
	row := func(id, projects string) string {
		return `{"id":"` + id + `","name":"kata.inbox.changed","recipient":"r","state":"active","refresh_before":null,"cursor":"c","continuity_generation":1,"gap_unacked":false,"delivery":{"active":true},"failed_attempts":0,"dead_letter_count":0` + projects + `}`
	}
	body := `{"enabled":true,"subscriptions":[` +
		row("in_scope", `,"project_uids":["`+testProjectUID+`"]`) + `,` +
		row("out_of_scope", `,"project_uids":["`+other+`"]`) + `,` +
		row("partly_out", `,"project_uids":["`+testProjectUID+`","`+other+`"]`) + `,` +
		row("dynamic", ``) + `]}`
	ids := func(scope *Scope) []string {
		daemon, client := newRecordingDaemon(t)
		daemon.reply = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}
		session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test", Events: true})
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.event_subscriptions", Arguments: map[string]any{}})
		require.NoError(t, err)
		require.False(t, result.IsError, mustJSON(t, result))
		var out []string
		for _, sub := range result.StructuredContent.(map[string]any)["subscriptions"].([]any) {
			out = append(out, sub.(map[string]any)["id"].(string))
		}
		return out
	}
	assert.Equal(t, []string{"in_scope"}, ids(boundTestScope(t)))
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: testProjectUID, Name: "a"}, {ID: 2, UID: other, Name: "b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"in_scope", "out_of_scope", "partly_out"}, ids(allowlist))
	assert.Equal(t, []string{"in_scope", "out_of_scope", "partly_out", "dynamic"}, ids(NewAllScope()))
}

func TestSharedToolAdmissionCapsBothServers(t *testing.T) {
	_, client := newRecordingDaemon(t)
	admission := newToolAdmission(rate.Every(time.Hour), 2, 8)
	options := Options{Client: client, Scope: boundTestScope(t), Actor: "example-agent", Version: "test", Admission: admission}
	first := connectRawTestServerWithOptions(t, options)
	options.Events = true
	second := connectRawTestServerWithOptions(t, options)
	call := func(session *sdkmcp.ClientSession) error {
		_, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: sectionLoaderNames[0], Arguments: map[string]any{}})
		return err
	}
	// The admitted calls wait on no deadline: runner speed cannot fail them.
	require.NoError(t, call(first))
	require.NoError(t, call(second))
	// The burst of two is spent across both servers, not doubled. The third
	// call needs a token that arrives in an hour, so the shared limiter refuses
	// it at once instead of waiting; only that call carries a deadline.
	refused, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	admit := toolAdmissionMiddleware(admission.limiter, admission.concurrent, nil)(func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
		return nil, errors.New("the refused call must not reach the tool")
	})
	_, err := admit(refused, "tools/call", nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "would exceed context deadline")
}

// Subscribing waits for the daemon's callback verification, which may take
// longer than the ordinary per-request timeout. The ordinary client here
// fails every request, so only the long-running client can succeed.
func TestSubscribeOutlivesTheOrdinaryRequestTimeout(t *testing.T) {
	server := httptest.NewServer(&recordingDaemon{})
	t.Cleanup(server.Close)
	ordinary := *server.Client()
	ordinary.Timeout = time.Nanosecond
	client, err := kataclient.NewWithHTTPClient(server.URL, &ordinary)
	require.NoError(t, err)
	longRunning, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	current, err := New(Options{
		Events: true, Client: client, LongRunningClient: longRunning, Scope: boundTestScope(t), Actor: "example-agent", Version: "test-version",
	})
	require.NoError(t, err)

	recorder, wire := serve(NewHTTPHandler(current, current), currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe", eventsParams(""))))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	result, ok := wire["result"].(map[string]any)
	require.True(t, ok, recorder.Body.String())
	assert.Equal(t, "sub_fixture", result["id"])
}

package mcpserver

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireHeaderMismatch(t *testing.T, recorder *httptest.ResponseRecorder, wire map[string]any) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	errObj, ok := wire["error"].(map[string]any)
	require.True(t, ok, recorder.Body.String())
	assert.InDelta(t, -32020.0, errObj["code"], 0.5)
}

func TestEventsNameHeader(t *testing.T) {
	encoded := "=?base64?" + base64.StdEncoding.EncodeToString([]byte(testEventName)) + "?="
	cases := []struct {
		desc   string
		name   string
		reject bool
	}{
		{"missing", "", true},
		{"mismatched", "other.name", true},
		{"equal", testEventName, false},
		{"base64 sentinel", encoded, false},
		{"malformed base64", "=?base64?!!!?=", true},
		{"base64 of another name", "=?base64?" + base64.StdEncoding.EncodeToString([]byte("other.name")) + "?=", true},
	}
	for _, method := range []string{"events/subscribe", "events/unsubscribe"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.desc, func(t *testing.T) {
				daemon, client := newRecordingDaemon(t)
				handler := newTestHTTPHandler(t, client, boundTestScope(t))
				recorder, wire := serve(handler, currentRequest(method, tc.name, eventsRPC(method, eventsParams(""))))
				if tc.reject {
					requireHeaderMismatch(t, recorder, wire)
					assert.InDelta(t, 7.0, wire["id"], 0.5)
					assert.Zero(t, daemon.eventCalls())
					return
				}
				assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			})
		}
	}
	t.Run("events/list needs no name", func(t *testing.T) {
		_, client := newRecordingDaemon(t)
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, wire := serve(handler, currentRequest("events/list", "", eventsRPC("events/list", "")))
		assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.Nil(t, wire["error"], recorder.Body.String())
	})
}

func TestEventsNameHeaderCannotBeBypassedByBodyAliases(t *testing.T) {
	params := eventsParams("")
	for _, tc := range []struct {
		desc string
		body string
		name string
	}{
		{"case-variant method member", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","METHOD":"tools/list","params":{` + currentMetaJSON + `,` + params + `}}`, ""},
		{"case-variant params member", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,` + params + `},"PARAMS":{"name":"other"}}`, "other"},
		{"case-variant params only", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","PARAMS":{` + currentMetaJSON + `,"name":"` + testEventName + `"}}`, testEventName},
		{"duplicate params", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{"name":"other"},"params":{` + currentMetaJSON + `,` + params + `}}`, testEventName},
		{"duplicate name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,"name":"other",` + params + `}}`, testEventName},
		{"duplicate name header matches first", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,"name":"other",` + params + `}}`, "other"},
		{"missing name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `}}`, testEventName},
		{"non-string name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,"name":5}}`, "5"},
		{"null name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,"name":null}}`, "null"},
		{"empty name", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":{` + currentMetaJSON + `,"name":""}}`, "=?base64??="},
		{"byte order mark", "\ufeff" + eventsRPC("events/subscribe", params), testEventName},
		{"params array", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":[{"name":"` + testEventName + `"}]}`, testEventName},
		{"params null", `{"jsonrpc":"2.0","id":7,"method":"events/subscribe","params":null}`, testEventName},
		{"body array", `[` + eventsRPC("events/subscribe", params) + `]`, testEventName},
		{"two objects", eventsRPC("events/subscribe", params) + eventsRPC("events/subscribe", params), testEventName},
		{"body string", `"events/subscribe"`, testEventName},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			daemon, client := newRecordingDaemon(t)
			handler := newTestHTTPHandler(t, client, boundTestScope(t))
			recorder, wire := serve(handler, currentRequest("events/subscribe", tc.name, tc.body))
			requireHeaderMismatch(t, recorder, wire)
			assert.Zero(t, daemon.eventCalls())
		})
	}
	t.Run("control: a valid request reaches the daemon", func(t *testing.T) {
		daemon, client := newRecordingDaemon(t)
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, _ := serve(handler, currentRequest("events/subscribe", testEventName, eventsRPC("events/subscribe", params)))
		assert.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.Equal(t, 1, daemon.eventCalls())
	})
}

func TestEventsNameHeaderBodyBoundary(t *testing.T) {
	padded := func(total int) string {
		base := eventsRPC("events/subscribe", eventsParams(`"pad":""`))
		return strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("a", total-len(base))+`"`, 1)
	}
	t.Run("exactly 1 MiB passes the guard", func(t *testing.T) {
		body := padded(1 << 20)
		require.Len(t, body, 1<<20)
		daemon, client := newRecordingDaemon(t)
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, _ := serve(handler, currentRequest("events/subscribe", testEventName, body))
		assert.NotEqual(t, http.StatusRequestEntityTooLarge, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "header mismatch")
		// The unknown "pad" member is rejected by the closed schema, not the guard.
		assert.Zero(t, daemon.eventCalls())
	})
	t.Run("1 MiB plus one byte is 413", func(t *testing.T) {
		body := padded(1<<20 + 1)
		require.Len(t, body, 1<<20+1)
		daemon, client := newRecordingDaemon(t)
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, _ := serve(handler, currentRequest("events/subscribe", testEventName, body))
		assert.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
		assert.Contains(t, recorder.Body.String(), "request body exceeds 1048576 bytes")
		assert.Zero(t, daemon.eventCalls())
	})
}

type recordingBody struct {
	r     io.Reader
	reads int
}

func (b *recordingBody) Read(p []byte) (int, error) { b.reads++; return b.r.Read(p) }
func (b *recordingBody) Close() error               { return nil }

func TestEventsNameHeaderSkipsBodyForOtherMethods(t *testing.T) {
	large := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x","pad":"` + strings.Repeat("a", 3<<20) + `"}}`
	small := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	for _, tc := range []struct{ method, body string }{
		{"tools/list", small},
		{"tools/call", large},
		{"events/list", small},
	} {
		t.Run(tc.method+"/"+strconv.Itoa(len(tc.body)), func(t *testing.T) {
			body := &recordingBody{r: strings.NewReader(tc.body)}
			var inner io.ReadCloser
			var readsBefore int
			next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				inner = r.Body
				readsBefore = body.reads
			})
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request.Body = body
			request.Header.Set("Mcp-Method", tc.method)
			eventsNameHeaderHandler(next).ServeHTTP(httptest.NewRecorder(), request)
			require.NotNil(t, inner)
			assert.Same(t, body, inner)
			assert.Zero(t, readsBefore)
		})
	}
	t.Run("a large tools/call reaches the SDK through the real handler", func(t *testing.T) {
		_, client := newRecordingDaemon(t)
		handler := newTestHTTPHandler(t, client, boundTestScope(t))
		recorder, _ := serve(handler, currentRequest("tools/call", "x", large))
		assert.NotEqual(t, http.StatusRequestEntityTooLarge, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "header mismatch: Mcp-Name")
	})
}

func TestEventsNameHeaderRelyOnSDKMethodHeaderValidation(t *testing.T) {
	// Mcp-Method claims tools/call, so the guard stands aside. The SDK's own
	// Mcp-Method validation must reject the lie before any Events handler runs;
	// if an SDK upgrade drops that check, this test fails.
	daemon, client := newRecordingDaemon(t)
	handler := newTestHTTPHandler(t, client, boundTestScope(t))
	recorder, wire := serve(handler, currentRequest("tools/call", "x", eventsRPC("events/subscribe", eventsParams(""))))
	requireHeaderMismatch(t, recorder, wire)
	assert.Zero(t, daemon.eventCalls())
	recorder, wire = serve(handler, func() *http.Request {
		request := currentRequest("", "", eventsRPC("events/subscribe", eventsParams("")))
		request.Header.Del("Mcp-Method")
		return request
	}())
	requireHeaderMismatch(t, recorder, wire)
	assert.Zero(t, daemon.eventCalls())
}

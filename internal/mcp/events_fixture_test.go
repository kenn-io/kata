package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

const (
	testProjectUID = "01ARZ3NDEKTSV4RRFFQ69G5FAA"
	testEventName  = "kata.inbox.changed"
	testCallback   = "https://receiver.example/inbox"
	testSecret     = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
)

// recordingDaemon stands in for the daemon's MCP Events routes. It records
// every request so tests can prove a rejected MCP request never reached it.
type recordingDaemon struct {
	mu       sync.Mutex
	requests []recordedRequest
	// reply, when set, answers instead of the canned success bodies.
	reply func(http.ResponseWriter, *http.Request)
}

type recordedRequest struct {
	Method string
	Path   string
	Body   map[string]any
}

func (d *recordingDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	d.mu.Lock()
	d.requests = append(d.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
	reply := d.reply
	d.mu.Unlock()
	if reply != nil {
		reply(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/mcp-events/list":
		_, _ = io.WriteString(w, `{"events":[{"name":"kata.inbox.changed","description":"d","delivery":["webhook"],"input_schema":{"type":"object"},"payload_schema":{"type":"object"}}]}`)
	case "/api/v1/mcp-events/subscribe":
		_, _ = io.WriteString(w, `{"id":"sub_fixture","refresh_before":null,"cursor":"c1.x","truncated":false,"delivery_status":{"active":true}}`)
	case "/api/v1/mcp-events/unsubscribe":
		_, _ = io.WriteString(w, `{"unsubscribed":true}`)
	case "/api/v1/mcp-events/subscriptions":
		_, _ = io.WriteString(w, `{"enabled":true,"subscriptions":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *recordingDaemon) calls() []recordedRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recordedRequest(nil), d.requests...)
}

func (d *recordingDaemon) eventCalls() int {
	count := 0
	for _, call := range d.calls() {
		if strings.HasPrefix(call.Path, "/api/v1/mcp-events/") {
			count++
		}
	}
	return count
}

func newRecordingDaemon(t *testing.T) (*recordingDaemon, *kataclient.Client) {
	t.Helper()
	daemon := &recordingDaemon{}
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	return daemon, client
}

// newTestHTTPHandler builds the production two-server handler: the legacy
// session server never serves Events, the stateless one does.
func newTestHTTPHandler(t *testing.T, client *kataclient.Client, scope *Scope) http.Handler {
	t.Helper()
	options := Options{
		Client: client, Scope: scope, Actor: "example-agent", Version: "test-version",
	}
	legacy, err := New(options)
	require.NoError(t, err)
	options.Events = true
	current, err := New(options)
	require.NoError(t, err)
	return NewHTTPHandler(legacy, current)
}

func boundTestScope(t *testing.T) *Scope {
	t.Helper()
	scope, err := NewBoundScope(ProjectIdentity{ID: 1, UID: testProjectUID, Name: "spoke-project"})
	require.NoError(t, err)
	return scope
}

const currentMetaJSON = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func eventsParams(extra string) string {
	body := `"name":"kata.inbox.changed","arguments":{"for":"example-agent/worker"},"delivery":{"mode":"webhook","url":"` + testCallback + `","secret":"` + testSecret + `"}`
	if extra != "" {
		body += "," + extra
	}
	return body
}

func eventsRPC(method, params string) string {
	if params != "" {
		params = "," + params
	}
	return `{"jsonrpc":"2.0","id":7,"method":"` + method + `","params":{` + currentMetaJSON + params + `}}`
}

// currentRequest builds a request for the 2026-07-28 stateless path.
func currentRequest(method, name, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", method)
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	return request
}

func serve(handler http.Handler, request *http.Request) (*httptest.ResponseRecorder, map[string]any) {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var wire map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &wire)
	return recorder, wire
}

func itoa(value int) string { return strconv.Itoa(value) }

func jsonUnmarshal(raw string, out any) error { return json.Unmarshal([]byte(raw), out) }

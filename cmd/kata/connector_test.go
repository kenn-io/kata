package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

type externalCLIRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

type externalCLIFixture struct {
	t              *testing.T
	server         *httptest.Server
	mu             sync.Mutex
	requests       []externalCLIRequest
	delay          bool
	connectorDelay bool
	failures       map[string]externalCLIFailure
}

type externalCLIFailure struct {
	status int
	body   map[string]any
	raw    []byte
}

func newExternalCLIFixture(t *testing.T) *externalCLIFixture {
	t.Helper()
	f := &externalCLIFixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *externalCLIFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	delayBridge := f.delay && (strings.Contains(r.URL.Path, "/bridge/actions/") || r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bridge"))
	delayConnector := f.connectorDelay && (r.URL.Path == "/api/v1/connectors" ||
		r.URL.Path == "/api/v1/connectors/example-connector" ||
		r.URL.Path == "/api/v1/connectors/example-connector/fields" ||
		r.Method == http.MethodPut && r.URL.Path == "/api/v1/connectors/example-connector/fields/scheduled_on")
	if delayBridge || delayConnector {
		select {
		case <-r.Context().Done():
			return
		case <-timeAfterForExternalCLITest():
		}
	}
	var body map[string]any
	if r.Body != nil {
		data, err := io.ReadAll(r.Body)
		require.NoError(f.t, err)
		if len(data) > 0 {
			require.NoError(f.t, json.Unmarshal(data, &body))
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, externalCLIRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if failure, ok := f.failures[r.Method+" "+r.URL.Path]; ok {
		w.WriteHeader(failure.status)
		if len(failure.raw) > 0 {
			_, _ = w.Write(failure.raw)
			return
		}
		writeExternalCLITestJSON(f.t, w, failure.body)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/ping":
		writeExternalCLITestJSON(f.t, w, map[string]any{"ok": true, "service": "kata", "version": "test"})
	case r.URL.Path == "/api/v1/projects/resolve":
		writeExternalCLITestJSON(f.t, w, map[string]any{"project": map[string]any{"id": 42, "name": "example-project"}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connectors":
		writeExternalCLITestJSON(f.t, w, connectorListFixture())
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connectors/example-connector":
		writeExternalCLITestJSON(f.t, w, connectorFixture())
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/connectors/example-connector/fields":
		writeExternalCLITestJSON(f.t, w, connectorFieldsFixture())
	case r.Method == http.MethodPut && r.URL.Path == "/api/v1/connectors/example-connector/fields/scheduled_on":
		writeExternalCLITestJSON(f.t, w, connectorMappingFixture(true))
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/connectors/example-connector/fields/scheduled_on":
		writeExternalCLITestJSON(f.t, w, connectorMappingFixture(false))
	case strings.HasPrefix(r.URL.Path, "/api/v1/projects/42/issues/abc4/bridge"):
		writeExternalCLITestJSON(f.t, w, bridgeFixtureForRequest(r, body))
	default:
		w.WriteHeader(http.StatusNotFound)
		writeExternalCLITestJSON(f.t, w, map[string]any{"error": map[string]any{"code": "not_found", "message": "safe resource not found"}})
	}
}

func (f *externalCLIFixture) snapshotRequests() []externalCLIRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]externalCLIRequest(nil), f.requests...)
}

func (f *externalCLIFixture) resetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *externalCLIFixture) fail(method, path string, status int, body map[string]any) {
	f.t.Helper()
	if f.failures == nil {
		f.failures = make(map[string]externalCLIFailure)
	}
	f.failures[method+" "+path] = externalCLIFailure{status: status, body: body}
}

func (f *externalCLIFixture) failRaw(method, path string, status int, body string) {
	f.t.Helper()
	if f.failures == nil {
		f.failures = make(map[string]externalCLIFailure)
	}
	f.failures[method+" "+path] = externalCLIFailure{status: status, raw: []byte(body)}
}

func runExternalCLI(ctx context.Context, t *testing.T, f *externalCLIFixture, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs(args)
	cmd.SetContext(contextWithBaseURL(ctx, f.server.URL))
	err := cmd.Execute()
	return out.String(), err
}

func runExternalCLIWithErrorOutput(ctx context.Context, t *testing.T, f *externalCLIFixture, args ...string) (string, string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var stdout, stderr strings.Builder
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	cmd.SetContext(contextWithBaseURL(ctx, f.server.URL))
	err := cmd.Execute()
	if err != nil {
		emitRootError(&stderr, cmd, args, err, runEEntered)
	}
	return stdout.String(), stderr.String(), err
}

func writeExternalCLITestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func connectorFixture() map[string]any {
	return map[string]any{
		"instance_id": "example-connector", "connector_id": "generic.connector", "display_name": "Generic connector",
		"protocol": "kata.connector/v1", "account_identity": "example-account",
		"capabilities": []string{"publish_comment", "fields"}, "healthy": true,
	}
}

func connectorListFixture() map[string]any {
	return map[string]any{"connectors": []any{connectorFixture()}}
}

func connectorFieldsFixture() map[string]any {
	return map[string]any{"fields": []any{map[string]any{
		"id": "start-date", "display_name": "Start date", "accepted_kinds": []string{"date", "local_datetime"},
		"nullable": true, "writable": true, "schema_revision": "schema-v1",
	}}}
}

func connectorMappingFixture(active bool) map[string]any {
	return map[string]any{
		"kata_field": "scheduled_on", "external_field_id": "start-date", "external_field_name": "Start date",
		"accepted_kinds": []string{"date", "local_datetime"}, "nullable": true, "writable": true,
		"schema_revision": "schema-v1", "active": active,
	}
}

func TestConnectorRootHelpRegistersEveryCommand(t *testing.T) {
	root := newRootCmd()
	rootHelp := string(executeRoot(t, newRootCmd(), "--help"))
	assert.Contains(t, rootHelp, "connector")
	assert.Contains(t, rootHelp, "bridge")

	connector, _, err := root.Find([]string{"connector"})
	require.NoError(t, err)
	assert.Equal(t, "connector", connector.Name())
	for _, path := range [][]string{{"connector", "list"}, {"connector", "status"}, {"connector", "field"}, {"connector", "field", "list"}, {"connector", "field", "map"}, {"connector", "field", "unmap"}} {
		child, _, findErr := root.Find(path)
		name := path[len(path)-1]
		require.NoErrorf(t, findErr, "connector %s", name)
		assert.Equal(t, name, child.Name())
	}
	help := string(executeRoot(t, newRootCmd(), "connector", "--help"))
	for _, name := range []string{"list", "status", "field"} {
		assert.Contains(t, help, name)
	}
	fieldHelp := string(executeRoot(t, newRootCmd(), "connector", "field", "--help"))
	for _, name := range []string{"list", "map", "unmap"} {
		assert.Contains(t, fieldHelp, name)
	}
	for _, obsolete := range []string{"fields", "map-field", "unmap-field"} {
		found, remaining, findErr := root.Find([]string{"connector", obsolete})
		require.NoError(t, findErr)
		assert.NotEqual(t, obsolete, found.Name())
		assert.Contains(t, remaining, obsolete)
	}
}

func TestConnectorMapFieldUsesExactBidirectionalPayload(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"connector", "field", "map"})
	require.NoError(t, err)
	assert.Nil(t, cmd.Flags().Lookup("direction"))

	f := newExternalCLIFixture(t)
	out, err := runExternalCLI(context.Background(), t, f, "--agent", "connector", "field", "map", "example-connector", "scheduled_on", "--external", "start-date")
	require.NoError(t, err)
	assert.Contains(t, out, "instance=example-connector")
	assert.Contains(t, out, "field=scheduled_on")
	requests := f.snapshotRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, http.MethodPut, requests[0].Method)
	assert.Equal(t, "/api/v1/connectors/example-connector/fields/scheduled_on", requests[0].Path)
	assert.Equal(t, map[string]any{"external_field": "start-date"}, requests[0].Body)

	f.resetRequests()
	_, err = runExternalCLI(context.Background(), t, f, "connector", "field", "unmap", "example-connector", "scheduled_on")
	require.NoError(t, err)
	requests = f.snapshotRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, http.MethodDelete, requests[0].Method)
	assert.Equal(t, "/api/v1/connectors/example-connector/fields/scheduled_on", requests[0].Path)
	assert.Empty(t, requests[0].Query)
	assert.Nil(t, requests[0].Body)
}

func TestConnectorProcessCallsUseLongRunningClient(t *testing.T) {
	f := newExternalCLIFixture(t)
	f.connectorDelay = true
	t.Setenv("KATA_HTTP_TIMEOUT", "500ms")
	for _, args := range [][]string{
		{"connector", "list"},
		{"connector", "status", "example-connector"},
		{"connector", "field", "list", "example-connector"},
		{"connector", "field", "map", "example-connector", "scheduled_on", "--external", "start-date"},
		{"connector", "field", "unmap", "example-connector", "scheduled_on"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := runExternalCLI(ctx, t, f, args...)
		cancel()
		require.NoErrorf(t, err, "%v", args)
	}
}

func TestConnectorEveryCommandRendersHumanJSONAndAgent(t *testing.T) {
	f := newExternalCLIFixture(t)
	tests := []struct {
		name      string
		args      []string
		humanWant []string
		agentWant []string
		jsonWant  []string
	}{
		{name: "list", args: []string{"connector", "list"}, humanWant: []string{"example-connector", "healthy", "generic.connector"}, agentWant: []string{"instance=example-connector", "state=healthy", "connector_id=generic.connector"}, jsonWant: []string{`"connectors"`, `"instance_id":"example-connector"`, `"account_identity":"example-account"`}},
		{name: "status", args: []string{"connector", "status", "example-connector"}, humanWant: []string{"Generic connector", "healthy", "publish_comment"}, agentWant: []string{"instance=example-connector", "state=healthy", "connector_id=generic.connector"}, jsonWant: []string{`"instance_id":"example-connector"`, `"healthy":true`, `"account_identity":"example-account"`}},
		{name: "field-list", args: []string{"connector", "field", "list", "example-connector"}, humanWant: []string{"Start date", "date", "local_datetime"}, agentWant: []string{"instance=example-connector", "field=start-date", "writable=true"}, jsonWant: []string{`"fields"`, `"id":"start-date"`}},
		{name: "field-map", args: []string{"connector", "field", "map", "example-connector", "scheduled_on", "--external", "start-date"}, humanWant: []string{"scheduled_on", "Start date", "active"}, agentWant: []string{"instance=example-connector", "field=scheduled_on", "active=true"}, jsonWant: []string{`"kata_field":"scheduled_on"`, `"active":true`}},
		{name: "field-unmap", args: []string{"connector", "field", "unmap", "example-connector", "scheduled_on"}, humanWant: []string{"scheduled_on", "inactive"}, agentWant: []string{"instance=example-connector", "field=scheduled_on", "active=false"}, jsonWant: []string{`"kata_field":"scheduled_on"`, `"active":false`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []struct {
				flag string
				want []string
			}{{want: test.humanWant}, {flag: "--json", want: test.jsonWant}, {flag: "--agent", want: test.agentWant}} {
				args := append([]string(nil), test.args...)
				if mode.flag != "" {
					args = append([]string{mode.flag}, args...)
				}
				out, err := runExternalCLI(context.Background(), t, f, args...)
				require.NoError(t, err)
				for _, want := range mode.want {
					assert.Contains(t, out, want)
				}
				if mode.flag == "--json" {
					assert.Contains(t, out, `"kata_api_version":1`)
				}
			}
		})
	}
}

func TestConnectorHumanOutputContainsOnlyPublicStatus(t *testing.T) {
	f := newExternalCLIFixture(t)
	for _, mode := range []string{"", "--agent"} {
		for _, command := range [][]string{{"connector", "list"}, {"connector", "status", "example-connector"}} {
			args := append([]string(nil), command...)
			if mode != "" {
				args = append([]string{mode}, args...)
			}
			out, err := runExternalCLI(context.Background(), t, f, args...)
			require.NoError(t, err)
			for _, forbidden := range []string{"settings", "command", "arguments", "environment", "account_identity", "example-account"} {
				assert.NotContains(t, out, forbidden)
			}
		}
	}
}

func TestConnectorMissingInstanceKeepsSafeDaemonError(t *testing.T) {
	f := newExternalCLIFixture(t)
	f.fail(http.MethodGet, "/api/v1/connectors/example-connector", http.StatusNotFound, map[string]any{
		"error": map[string]any{"code": "connector_not_found", "message": "connector instance was not found", "detail": "diagnostic-sentinel"},
	})
	_, err := runExternalCLI(context.Background(), t, f, "connector", "status", "example-connector")
	require.Error(t, err)
	var cli *cliError
	require.ErrorAs(t, err, &cli)
	assert.Equal(t, kindNotFound, cli.Kind)
	assert.Equal(t, ExitNotFound, cli.ExitCode)
	assert.Equal(t, "connector_not_found", cli.Code)
	assert.Equal(t, "connector instance was not found", cli.Message)
	assert.NotContains(t, cli.Error(), "diagnostic-sentinel")
}

func TestExternalCommandsRedactMalformedErrorBodiesInEveryOutputMode(t *testing.T) {
	const rawDiagnostic = "upstream private diagnostic: credential=diagnostic-sentinel root_binding=opaque-private-value"
	tests := []struct {
		name    string
		method  string
		path    string
		status  int
		body    string
		args    []string
		message string
		kind    errKind
		exit    int
	}{
		{
			name: "connector", method: http.MethodGet, path: "/api/v1/connectors/example-connector", status: http.StatusBadGateway,
			body: `{"error":{"message":"` + rawDiagnostic,
			args: []string{"connector", "status", "example-connector"}, message: "external root request failed (HTTP 502 Bad Gateway)",
			kind: kindInternal, exit: ExitInternal,
		},
		{
			name: "bridge", method: http.MethodGet, path: "/api/v1/projects/42/issues/abc4/bridge", status: http.StatusNotFound,
			body: rawDiagnostic,
			args: []string{"bridge", "show", "example-project#abc4"}, message: "external root request failed (HTTP 404 Not Found)",
			kind: kindNotFound, exit: ExitNotFound,
		},
	}
	for _, test := range tests {
		for _, mode := range []string{"", "--json", "--agent"} {
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				f := newExternalCLIFixture(t)
				f.failRaw(test.method, test.path, test.status, test.body)
				args := append([]string(nil), test.args...)
				if mode != "" {
					args = append([]string{mode}, args...)
				}
				stdout, stderr, err := runExternalCLIWithErrorOutput(context.Background(), t, f, args...)
				assert.Empty(t, stdout)
				cli := requireCLIError(t, err, test.exit)
				assert.Equal(t, test.kind, cli.Kind)
				assert.Empty(t, cli.Code)
				assert.Equal(t, test.message, cli.Message)
				assert.Contains(t, stderr, test.message)
				for _, forbidden := range []string{rawDiagnostic, "credential=", "root_binding=", "diagnostic-sentinel", "opaque-private-value"} {
					assert.NotContains(t, stderr, forbidden)
					assert.NotContains(t, cli.Error(), forbidden)
				}
				if mode == "--json" {
					envelope := parseErrorEnvelope(t, []byte(stderr))
					assert.Equal(t, string(test.kind), envelope.Error.Kind)
					assert.Equal(t, test.message, envelope.Error.Message)
					assert.Equal(t, test.exit, envelope.Error.ExitCode)
				}
			})
		}
	}
}

func TestExternalCLIResponseErrorPreservesTransportError(t *testing.T) {
	transportErr := errors.New("transport-sentinel")
	assert.Same(t, transportErr, externalCLIResponseError(0, nil, transportErr))
}

func TestExternalCLITransportGuardAndConcreteEnvelopeHandling(t *testing.T) {
	transportErr := errors.New("transport-sentinel")
	assert.Same(t, transportErr, externalCLITransportError((*generated.GetConnectorStatusResp)(nil), transportErr))

	err := externalCLITransportError((*generated.GetExternalRootBridgeResp)(nil), nil)
	cli := requireCLIError(t, err, ExitInternal)
	assert.Equal(t, kindInternal, cli.Kind)
	assert.Equal(t, "external root request returned no response", cli.Message)

	generatedStatusErr := errors.New("generated status error")
	connectorResp := &generated.GetConnectorStatusResp{
		StatusCode: http.StatusNotFound,
		Body:       []byte(`{"error":{"code":"connector_not_found","message":"connector instance was not found"}}`),
	}
	require.NoError(t, externalCLITransportError(connectorResp, generatedStatusErr))
	connectorErr := externalCLIResponseError(connectorResp.StatusCode, connectorResp.Body, generatedStatusErr)
	connectorCLI := requireCLIError(t, connectorErr, ExitNotFound)
	assert.Equal(t, kindNotFound, connectorCLI.Kind)
	assert.Equal(t, "connector_not_found", connectorCLI.Code)
	assert.Equal(t, "connector instance was not found", connectorCLI.Message)

	bridgeResp := &generated.GetExternalRootBridgeResp{
		StatusCode: http.StatusConflict,
		Body:       []byte(`{"error":{"code":"external_root_claim_lost","message":"external root bridge is busy or unavailable"}}`),
	}
	require.NoError(t, externalCLITransportError(bridgeResp, generatedStatusErr))
	bridgeErr := externalCLIResponseError(bridgeResp.StatusCode, bridgeResp.Body, generatedStatusErr)
	bridgeCLI := requireCLIError(t, bridgeErr, ExitConflict)
	assert.Equal(t, kindConflict, bridgeCLI.Kind)
	assert.Equal(t, "external_root_claim_lost", bridgeCLI.Code)
	assert.Equal(t, "external root bridge is busy or unavailable", bridgeCLI.Message)
}

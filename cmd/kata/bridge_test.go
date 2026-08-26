package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/pkg/client/generated"
)

func timeAfterForExternalCLITest() <-chan time.Time { return time.After(750 * time.Millisecond) }

func bridgeFixtureForRequest(r *http.Request, requestBody map[string]any) map[string]any {
	bridge := map[string]any{
		"id": 7, "uid": "01JBRIDGE00000000000000000", "project_id": 42, "issue_id": 9,
		"connector_instance": "example-connector", "active": true, "enabled": true,
		"receive_comments": true, "publish_comments": false, "complete_external": true,
		"consecutive_failures": 1, "last_success_at": "2026-08-20T12:00:00Z",
		"last_error": "safe connector failure", "pending_comment_uid": "01JCOMMENT0000000000000000",
		"field_conflicts": []any{map[string]any{
			"kata_field":         "scheduled_on",
			"kata_candidate":     map[string]any{"kind": "date", "value": "2026-08-21"},
			"external_candidate": map[string]any{"kind": "local_datetime", "value": "2026-08-22T09:30:00", "timezone": "Etc/UTC"},
		}},
	}
	if r.Method == http.MethodDelete {
		bridge["active"] = false
		bridge["enabled"] = false
	}
	if publish, ok := requestBody["publish_comments"].(bool); ok {
		bridge["publish_comments"] = publish
	}
	if strings.HasSuffix(r.URL.Path, "/actions/pause") {
		bridge["enabled"] = false
		bridge["paused_reason"] = "operator pause"
	}
	if strings.HasSuffix(r.URL.Path, "/actions/reconcile") {
		return map[string]any{
			"bridge": bridge, "root_updated": true, "comments_created": 2, "comments_edited": 1,
			"field_conflicts": 1, "completion_requests": 0, "reopen_requests": 0, "paused": false,
		}
	}
	return bridge
}

func TestBridgeRootHelpRegistersEveryCommand(t *testing.T) {
	root := newRootCmd()
	bridge, _, err := root.Find([]string{"bridge"})
	require.NoError(t, err)
	assert.Equal(t, "bridge", bridge.Name())
	for _, name := range []string{"bind", "show", "reconcile", "pause", "resume", "resolve-field", "resolve-comment", "unbind"} {
		child, _, findErr := root.Find([]string{"bridge", name})
		require.NoErrorf(t, findErr, "bridge %s", name)
		assert.Equal(t, name, child.Name())
	}
	help := string(executeRoot(t, newRootCmd(), "bridge", "--help"))
	for _, name := range []string{"bind", "show", "reconcile", "pause", "resume", "resolve-field", "resolve-comment", "unbind"} {
		assert.Contains(t, help, name)
	}
}

func TestBridgeBindDefaultsToQuietOutboundComments(t *testing.T) {
	f := newExternalCLIFixture(t)
	out, err := runExternalCLI(context.Background(), t, f, "--agent", "--as", "operator", "bridge", "bind", "example-project#abc4", "--connector", "example-connector", "--external", "root-locator")
	require.NoError(t, err)
	assert.Contains(t, out, "receive_comments=true")
	assert.Contains(t, out, "publish_comments=false")
	assert.NotContains(t, out, "root-locator")
	requests := f.snapshotRequests()
	require.Len(t, requests, 2)
	assert.Equal(t, map[string]any{"name": "example-project"}, requests[0].Body)
	assert.Equal(t, map[string]any{
		"actor": "operator", "connector": "example-connector", "external": "root-locator", "publish_comments": false,
	}, requests[1].Body)

	for _, mode := range []struct {
		flag string
		want string
	}{
		{want: "Publish comments: true"},
		{flag: "--json", want: `"publish_comments":true`},
		{flag: "--agent", want: "publish_comments=true"},
	} {
		f.resetRequests()
		args := []string{"--as", "operator", "bridge", "bind", "example-project#abc4", "--connector", "example-connector", "--external", "root-locator", "--publish-comments"}
		if mode.flag != "" {
			args = append([]string{mode.flag}, args...)
		}
		out, err = runExternalCLI(context.Background(), t, f, args...)
		require.NoError(t, err)
		assert.Contains(t, out, mode.want)
		requests = f.snapshotRequests()
		require.Len(t, requests, 2)
		assert.Equal(t, true, requests[1].Body["publish_comments"])
	}
}

func TestBridgeEveryCommandRendersHumanJSONAndAgent(t *testing.T) {
	f := newExternalCLIFixture(t)
	tests := []struct {
		name      string
		args      []string
		humanWant []string
		agentWant []string
		jsonWant  []string
	}{
		{name: "bind", args: []string{"bridge", "bind", "example-project#abc4", "--connector", "example-connector", "--external", "root-locator"}, humanWant: []string{"example-connector", "enabled", "Receive comments: true", "Publish comments: false"}, agentWant: []string{"instance=example-connector", "state=enabled", "receive_comments=true", "publish_comments=false"}, jsonWant: []string{`"connector_instance":"example-connector"`, `"receive_comments":true`}},
		{name: "show", args: []string{"bridge", "show", "example-project#abc4"}, humanWant: []string{"Pending comment: 01JCOMMENT0000000000000000", "scheduled_on", "kind=date", "value=2026-08-21", "timezone=Etc/UTC"}, agentWant: []string{"instance=example-connector", "state=enabled", "pending_comment=01JCOMMENT0000000000000000", "field=scheduled_on", "kata_value=2026-08-21", "external_timezone=Etc/UTC"}, jsonWant: []string{`"pending_comment_uid":"01JCOMMENT0000000000000000"`, `"kata_candidate"`}},
		{name: "reconcile", args: []string{"bridge", "reconcile", "example-project#abc4"}, humanWant: []string{"Reconciled", "Comments created: 2", "Root updated: true"}, agentWant: []string{"state=enabled", "comments_created=2", "root_updated=true"}, jsonWant: []string{`"comments_created":2`, `"root_updated":true`}},
		{name: "pause", args: []string{"bridge", "pause", "example-project#abc4", "--reason", "operator pause"}, humanWant: []string{"paused", "operator pause"}, agentWant: []string{"state=paused", "instance=example-connector"}, jsonWant: []string{`"enabled":false`, `"paused_reason":"operator pause"`}},
		{name: "resume", args: []string{"bridge", "resume", "example-project#abc4"}, humanWant: []string{"enabled", "example-connector"}, agentWant: []string{"state=enabled", "instance=example-connector"}, jsonWant: []string{`"enabled":true`}},
		{name: "resolve-field", args: []string{"bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "kata"}, humanWant: []string{"scheduled_on", "kata", "enabled"}, agentWant: []string{"field=scheduled_on", "use=kata", "state=enabled"}, jsonWant: []string{`"connector_instance":"example-connector"`}},
		{name: "resolve-comment-adopt", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--adopt", "opaque comment/id"}, humanWant: []string{"comment", "adopt", "enabled"}, agentWant: []string{"resolution=adopt", "state=enabled"}, jsonWant: []string{`"connector_instance":"example-connector"`}},
		{name: "resolve-comment-retry", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--retry"}, humanWant: []string{"comment", "retry", "enabled"}, agentWant: []string{"resolution=retry", "state=enabled"}, jsonWant: []string{`"connector_instance":"example-connector"`}},
		{name: "resolve-comment-skip", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--skip"}, humanWant: []string{"comment", "skip", "enabled"}, agentWant: []string{"resolution=skip", "state=enabled"}, jsonWant: []string{`"connector_instance":"example-connector"`}},
		{name: "unbind", args: []string{"bridge", "unbind", "example-project#abc4"}, humanWant: []string{"unbound", "example-connector"}, agentWant: []string{"state=unbound", "instance=example-connector"}, jsonWant: []string{`"active":false`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []struct {
				flag string
				want []string
			}{{want: test.humanWant}, {flag: "--json", want: test.jsonWant}, {flag: "--agent", want: test.agentWant}} {
				args := append([]string{"--as", "operator"}, test.args...)
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

func TestBridgeOutputSortsConflictsAndRendersMissingValuesNeutrally(t *testing.T) {
	resetFlags(t)
	bridge := generated.ExternalRootBridgeOut{
		Active: true, Enabled: true, ConnectorInstance: "example-connector",
		FieldConflicts: []generated.ExternalFieldConflictOut{
			{KataField: "scheduled_on"},
			{KataField: "deadline_on", KataCandidate: &generated.ExternalFieldCandidateOut{Kind: "date"}},
		},
	}
	cmd := newBridgeShowCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	require.NoError(t, printBridgeResponse(cmd, nil, &bridge, "show", nil))
	human := out.String()
	assert.Contains(t, human, "Last success: -")
	assert.Contains(t, human, "Last error: -")
	assert.Contains(t, human, "Pending comment: -")
	assert.Contains(t, human, "Kata: kind=date value=- timezone=-")
	assert.Contains(t, human, "External: kind=- value=- timezone=-")
	assert.Less(t, strings.Index(human, "Field conflict: deadline_on"), strings.Index(human, "Field conflict: scheduled_on"))

	flags.Mode = outputAgent
	out.Reset()
	require.NoError(t, printBridgeResponse(cmd, nil, &bridge, "show", nil))
	agent := out.String()
	assert.Contains(t, agent, "pending_comment=- last_error=-")
	assert.Contains(t, agent, "kata_kind=date kata_value=- kata_timezone=-")
	assert.Contains(t, agent, "external_kind=- external_value=- external_timezone=-")
	assert.Less(t, strings.Index(agent, "field=deadline_on"), strings.Index(agent, "field=scheduled_on"))
}

func TestBridgeResolveCommentRequiresExactlyOneModeBeforeHTTP(t *testing.T) {
	for _, args := range [][]string{
		{"bridge", "resolve-comment", "example-project#abc4"},
		{"bridge", "resolve-comment", "example-project#abc4", "--retry", "--skip"},
		{"bridge", "resolve-comment", "example-project#abc4", "--adopt", "external-1", "--retry"},
	} {
		_, _, err := executeRootCapture(t, context.Background(), args...)
		require.Error(t, err)
		var cli *cliError
		require.ErrorAs(t, err, &cli)
		assert.Equal(t, kindValidation, cli.Kind)
		assert.Contains(t, cli.Message, "exactly one")
	}
}

func TestBridgeResolveCommentPreservesOpaqueAdoptID(t *testing.T) {
	f := newExternalCLIFixture(t)
	const opaque = "opaque ID/with spaces:%25"
	_, err := runExternalCLI(context.Background(), t, f, "--as", "operator", "bridge", "resolve-comment", "example-project#abc4", "--adopt", opaque)
	require.NoError(t, err)
	requests := f.snapshotRequests()
	require.Len(t, requests, 2)
	assert.Equal(t, map[string]any{"action": "adopt", "actor": "operator", "external_comment_id": opaque}, requests[1].Body)
}

func TestBridgeResolveFieldRejectsInvalidChoiceBeforeHTTP(t *testing.T) {
	_, _, err := executeRootCapture(t, context.Background(), "bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "newest")
	require.Error(t, err)
	var cli *cliError
	require.ErrorAs(t, err, &cli)
	assert.Equal(t, kindValidation, cli.Kind)
	assert.Contains(t, cli.Message, "kata or external")
}

func TestBridgeQualifiedRefUsesConfiguredRemoteDaemon(t *testing.T) {
	f := newExternalCLIFixture(t)
	home := setupKataEnv(t)
	t.Setenv("KATA_SERVER", f.server.URL)
	t.Setenv("KATA_HOME", home)
	out, _, err := executeRootCapture(t, context.Background(), "--agent", "bridge", "show", "example-project#abc4")
	require.NoError(t, err)
	assert.Contains(t, out, "instance=example-connector")
	requests := f.snapshotRequests()
	assert.Condition(t, func() bool {
		for _, request := range requests {
			if request.Path == "/api/v1/projects/42/issues/abc4/bridge" {
				return true
			}
		}
		return false
	})
}

func TestBridgeBareRefUsesExplicitProjectContext(t *testing.T) {
	f := newExternalCLIFixture(t)
	out, err := runExternalCLI(context.Background(), t, f, "--agent", "--project", "example-project", "bridge", "show", "abc4")
	require.NoError(t, err)
	assert.Contains(t, out, "instance=example-connector")
	requests := f.snapshotRequests()
	require.Len(t, requests, 2)
	assert.Equal(t, map[string]any{"name": "example-project"}, requests[0].Body)
	assert.Equal(t, "/api/v1/projects/42/issues/abc4/bridge", requests[1].Path)
}

func TestBridgeExternalCallsUseLongRunningClient(t *testing.T) {
	f := newExternalCLIFixture(t)
	f.delay = true
	t.Setenv("KATA_HTTP_TIMEOUT", "500ms")
	tests := [][]string{
		{"bridge", "bind", "example-project#abc4", "--connector", "example-connector", "--external", "root-locator"},
		{"bridge", "reconcile", "example-project#abc4"},
		{"bridge", "resume", "example-project#abc4"},
		{"bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "external"},
		{"bridge", "resolve-comment", "example-project#abc4", "--retry"},
	}
	for _, args := range tests {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := runExternalCLI(ctx, t, f, args...)
		cancel()
		require.NoErrorf(t, err, "%v", args)
	}
}

func TestBridgeLongRunningClientHonorsCancellation(t *testing.T) {
	f := newExternalCLIFixture(t)
	f.delay = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := runExternalCLI(ctx, t, f, "bridge", "reconcile", "example-project#abc4")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestBridgeCommandsSendExactQualifiedRefRequests(t *testing.T) {
	f := newExternalCLIFixture(t)
	tests := []struct {
		name   string
		args   []string
		method string
		path   string
		query  string
		body   map[string]any
	}{
		{name: "show", args: []string{"bridge", "show", "example-project#abc4"}, method: http.MethodGet, path: "/api/v1/projects/42/issues/abc4/bridge"},
		{name: "reconcile", args: []string{"bridge", "reconcile", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/reconcile", body: map[string]any{"actor": "operator"}},
		{name: "pause without reason", args: []string{"bridge", "pause", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/pause", body: map[string]any{"actor": "operator"}},
		{name: "pause", args: []string{"bridge", "pause", "example-project#abc4", "--reason", "operator pause"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/pause", body: map[string]any{"actor": "operator", "reason": "operator pause"}},
		{name: "resume", args: []string{"bridge", "resume", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resume", body: map[string]any{"actor": "operator"}},
		{name: "resolve-field", args: []string{"bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "external"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-field", body: map[string]any{"actor": "operator", "kata_field": "scheduled_on", "use": "external"}},
		{name: "resolve-comment adopt", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--adopt", "opaque ID/with spaces:%25"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-comment", body: map[string]any{"action": "adopt", "actor": "operator", "external_comment_id": "opaque ID/with spaces:%25"}},
		{name: "resolve-comment", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--retry"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-comment", body: map[string]any{"action": "retry", "actor": "operator"}},
		{name: "resolve-comment skip", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--skip"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-comment", body: map[string]any{"action": "skip", "actor": "operator"}},
		{name: "unbind", args: []string{"bridge", "unbind", "example-project#abc4"}, method: http.MethodDelete, path: "/api/v1/projects/42/issues/abc4/bridge", query: "actor=operator"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f.resetRequests()
			args := append([]string{"--as", "operator"}, test.args...)
			_, err := runExternalCLI(context.Background(), t, f, args...)
			require.NoError(t, err)
			requests := f.snapshotRequests()
			require.Len(t, requests, 2)
			assert.Equal(t, map[string]any{"name": "example-project"}, requests[0].Body)
			assert.Equal(t, test.method, requests[1].Method)
			assert.Equal(t, test.path, requests[1].Path)
			assert.Equal(t, test.query, requests[1].Query)
			assert.Equal(t, test.body, requests[1].Body)
		})
	}
}

func TestBridgeUnbindResolvesArchivedQualifiedProject(t *testing.T) {
	var requests []externalCLIRequest
	f := &externalCLIFixture{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, externalCLIRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/projects/resolve":
			w.WriteHeader(http.StatusNotFound)
			writeExternalCLITestJSON(t, w, map[string]any{"error": map[string]any{
				"code": "project_not_initialized", "message": "project is not active",
			}})
		case "GET /api/v1/projects":
			require.Equal(t, "archived", r.URL.Query().Get("include"))
			writeExternalCLITestJSON(t, w, map[string]any{"projects": []any{map[string]any{
				"id": 42, "uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "name": "archived-project",
				"deleted_at": "2026-08-22T12:00:00.000Z",
			}}})
		case "DELETE /api/v1/projects/42/issues/abc4/bridge":
			writeExternalCLITestJSON(t, w, bridgeFixtureForRequest(r, nil))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)

	_, err := runExternalCLI(context.Background(), t, f,
		"--as", "operator", "bridge", "unbind", "archived-project#abc4")
	require.NoError(t, err)
	require.Equal(t, []externalCLIRequest{
		{Method: http.MethodPost, Path: "/api/v1/projects/resolve"},
		{Method: http.MethodGet, Path: "/api/v1/projects", Query: "include=archived"},
		{Method: http.MethodDelete, Path: "/api/v1/projects/42/issues/abc4/bridge", Query: "actor=operator"},
	}, requests)
}

func TestBridgeErrorsKeepStableSafeEnvelopes(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		method  string
		path    string
		status  int
		code    string
		message string
		kind    errKind
		exit    int
	}{
		{name: "missing binding", args: []string{"bridge", "show", "example-project#abc4"}, method: http.MethodGet, path: "/api/v1/projects/42/issues/abc4/bridge", status: http.StatusNotFound, code: "bridge_not_found", message: "external root bridge not found", kind: kindNotFound, exit: ExitNotFound},
		{name: "validation", args: []string{"bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "kata"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-field", status: http.StatusBadRequest, code: "validation", message: "external root request is invalid", kind: kindValidation, exit: ExitValidation},
		{name: "field changed", args: []string{"bridge", "resolve-field", "example-project#abc4", "scheduled_on", "--use", "kata"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-field", status: http.StatusConflict, code: "external_field_conflict", message: "external field conflict changed", kind: kindConflict, exit: ExitConflict},
		{name: "pending comment", args: []string{"bridge", "resolve-comment", "example-project#abc4", "--skip"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resolve-comment", status: http.StatusConflict, code: "external_comment_pending", message: "external comment requires explicit resolution", kind: kindConflict, exit: ExitConflict},
		{name: "capability conflict", args: []string{"bridge", "reconcile", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/reconcile", status: http.StatusConflict, code: "external_root_conflict", message: "external root operation conflicts with connector capabilities", kind: kindConflict, exit: ExitConflict},
		{name: "claim loss", args: []string{"bridge", "resume", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/resume", status: http.StatusConflict, code: "external_root_claim_lost", message: "external root bridge is busy or unavailable", kind: kindConflict, exit: ExitConflict},
		{name: "connector failure", args: []string{"bridge", "reconcile", "example-project#abc4"}, method: http.MethodPost, path: "/api/v1/projects/42/issues/abc4/bridge/actions/reconcile", status: http.StatusBadGateway, code: "connector_error", message: "external connector request failed", kind: kindInternal, exit: ExitInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalCLIFixture(t)
			f.fail(test.method, test.path, test.status, map[string]any{
				"error": map[string]any{"code": test.code, "message": test.message, "detail": "diagnostic-sentinel"},
			})
			_, err := runExternalCLI(context.Background(), t, f, append([]string{"--as", "operator"}, test.args...)...)
			require.Error(t, err)
			var cli *cliError
			require.ErrorAs(t, err, &cli)
			assert.Equal(t, test.code, cli.Code)
			assert.Equal(t, test.message, cli.Message)
			assert.Equal(t, test.kind, cli.Kind)
			assert.Equal(t, test.exit, cli.ExitCode)
			assert.NotContains(t, cli.Error(), "diagnostic-sentinel")
		})
	}
}

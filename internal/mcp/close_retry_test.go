package mcpserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCloseForwardsRetryGuardsAndReturnsOriginalReceipt(t *testing.T) {
	var idempotencyKey, ifMatch, retryProtocol string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			idempotencyKey = request.Header.Get("Idempotency-Key")
			ifMatch = request.Header.Get("If-Match")
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			retryProtocol, _ = body["retry_protocol"].(string)
			issue := issueJSON(1, "spoke-project", "abc1")
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue":          issue,
				"event":          nil,
				"original_event": closeRetryEventJSON(),
				"changed":        false,
				"reused":         true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}
	revision := int64(7)
	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1", Revision: &revision,
	})
	require.NoError(t, err)
	require.Equal(t, "close-request-1", idempotencyKey)
	require.Equal(t, `"rev-7"`, ifMatch)
	require.Equal(t, "close-v1", retryProtocol)
	require.NotNil(t, output.Reused)
	require.True(t, *output.Reused)
	require.NotNil(t, output.Event)
	require.Equal(t, "01HCCCCCCCCCCCCCCCCCCCCCCC", output.Event.UID)
}

// A close receipt must fail closed when its returned issue cannot be projected
// into the MCP startup scope. Warning projection must not erase that error.
func TestClosePreservesMutationProjectionError(t *testing.T) {
	const projectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, projectUID, "spoke-project")}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "other-project", "abc1")
			issue["project_uid"] = projectUID
			writeJSON(writer, map[string]any{"issue": issue, "changed": false})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: projectUID, Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope, Actor: "example-agent"}}
	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message: "Reviewed the request and recorded why the work should stop here.",
	})
	require.ErrorContains(t, err, "outside the MCP startup scope")
}

func TestAuditClosesRedactsForeignVerdictReferences(t *testing.T) {
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/audit/closes":
			writeJSON(writer, map[string]any{"rows": []any{map[string]any{
				"time": "2026-08-12T00:00:00Z", "actor": "worker", "reason": "done", "issue": "abc1", "event_id": 1,
				"overruled_refutations": []string{"c:abc123", "other-project#def2:def456", "01HAAAAAAAAAAAAAAAAAAAAAAA"},
				"verdict_evidence":      []any{map[string]any{"type": "confirmed", "comment": "c:abc123"}, map[string]any{"type": "confirmed", "comment": "other-project#def2:def456"}},
			}}})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, out, err := handlers.auditCloses(t.Context(), nil, AuditClosesInput{Project: "spoke-project"})
	require.NoError(t, err)
	require.Len(t, out.Rows, 1)
	require.Equal(t, []string{"c:abc123"}, out.Rows[0].OverruledRefutations)
	require.Len(t, out.Rows[0].VerdictEvidence, 1)
	require.Equal(t, "c:abc123", *out.Rows[0].VerdictEvidence[0].Comment)
}

func TestCloseRejectsReusedIssueMovedOutsideFixedScope(t *testing.T) {
	const (
		allowedProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		foreignProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, allowedProjectUID, "spoke-project"),
				projectJSON(2, foreignProjectUID, "other-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "other-project", "def1")
			issue["project_uid"] = foreignProjectUID
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue": issue, "original_event": closeRetryEventJSON(),
				"changed": false, "reused": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 1, UID: allowedProjectUID, Name: "spoke-project",
	}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{
		Client: client, Scope: scope, Actor: "example-agent",
	}}

	_, _, err = handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.ErrorContains(t, err, "outside the MCP startup scope")
}

func TestCloseUsesCurrentProjectForReusedIssueMovedWithinScope(t *testing.T) {
	const (
		sourceProjectUID = "01HAAAAAAAAAAAAAAAAAAAAAAA"
		targetProjectUID = "01HBBBBBBBBBBBBBBBBBBBBBBB"
	)
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, sourceProjectUID, "source-project"),
				projectJSON(2, targetProjectUID, "target-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			issue := issueJSON(2, "target-project", "def1")
			issue["project_uid"] = targetProjectUID
			issue["status"] = "closed"
			writeJSON(writer, map[string]any{
				"issue": issue, "original_event": closeRetryEventJSON(),
				"changed": false, "reused": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: sourceProjectUID, Name: "source-project"},
		{ID: 2, UID: targetProjectUID, Name: "target-project"},
	})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{
		Client: client, Scope: scope, Actor: "example-agent",
	}}

	_, output, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "source-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.NoError(t, err)
	require.Equal(t, ProjectIdentity{ID: 2, UID: targetProjectUID, Name: "target-project"}, output.Project)
	require.Equal(t, "target-project#def1", output.Issue.QualifiedRef)
}

func TestCloseRetryProtocolMakesLegacyDaemonRejectRequest(t *testing.T) {
	var closeCalls, mutations int
	var retryProtocol string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			closeCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			retryProtocol, _ = body["retry_protocol"].(string)
			if retryProtocol != "" {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusBadRequest)
				writeJSON(writer, map[string]any{
					"status": http.StatusBadRequest,
					"error": map[string]any{
						"code": "validation", "message": "retry_protocol: unexpected property",
					},
				})
				return
			}
			mutations++
			writeJSON(writer, map[string]any{
				"issue": issueJSON(1, "spoke-project", "abc1"), "changed": true,
			})
		default:
			http.NotFound(writer, request)
		}
	})
	handlers := toolHandlers{options: Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent",
	}}
	_, _, err := handlers.close(t.Context(), nil, CloseInput{
		Ref: "spoke-project#abc1", Reason: "wontfix",
		Message:        "Reviewed the request and recorded why the work should stop here.",
		IdempotencyKey: "close-request-1",
	})
	require.Error(t, err)
	require.Equal(t, "close-v1", retryProtocol)
	require.Equal(t, 1, closeCalls)
	require.Zero(t, mutations)
}

func closeRetryEventJSON() map[string]any {
	return map[string]any{
		"id": 9, "uid": "01HCCCCCCCCCCCCCCCCCCCCCCC",
		"origin_instance_uid": "01HDDDDDDDDDDDDDDDDDDDDDDD",
		"project_id":          1, "project_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA",
		"project_name": "spoke-project", "type": "issue.closed",
		"actor": "example-agent", "payload": `{}`,
		"hlc_physical_ms": 1, "hlc_counter": 0,
		"content_hash": strings.Repeat("a", 64),
		"created_at":   "2026-08-11T00:00:00Z",
	}
}

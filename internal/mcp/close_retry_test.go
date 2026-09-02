package mcpserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCloseForwardsRetryGuardsAndReturnsOriginalReceipt(t *testing.T) {
	var idempotencyKey, ifMatch string
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			idempotencyKey = request.Header.Get("Idempotency-Key")
			ifMatch = request.Header.Get("If-Match")
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
		CloseRetryHeadersSupported: true,
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
	require.NotNil(t, output.Reused)
	require.True(t, *output.Reused)
	require.NotNil(t, output.Event)
	require.Equal(t, "01HCCCCCCCCCCCCCCCCCCCCCCC", output.Event.UID)
}

func TestCloseRejectsRetryGuardsWhenDaemonDoesNotSupportThem(t *testing.T) {
	var closeCalls int
	client := reviewClient(t, func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/projects":
			writeJSON(writer, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
		case strings.HasSuffix(request.URL.Path, "/actions/close"):
			closeCalls++
			writeJSON(writer, map[string]any{"changed": true})
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
	require.ErrorContains(t, err, "daemon does not support kata.close retry controls")
	require.Zero(t, closeCalls)
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

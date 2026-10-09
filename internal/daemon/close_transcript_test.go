package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestCloseTranscript_PersistenceAndRetries(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(projectID, issueID, "actions/close")
	transcript := map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001", "url": "https://agentsview.example/sessions/codex/00000000-0000-4000-8000-000000000001"}
	body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "transcript": transcript, "retry_protocol": "close-v1"}
	headers := map[string]string{"Idempotency-Key": "transcript-close-one"}
	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var out api.MutationResponse
	require.NoError(t, json.Unmarshal(first.body, &out.Body))
	require.NotNil(t, out.Body.Event)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(out.Body.Event.Payload), &payload))
	assert.Equal(t, transcript, payload["transcript"])
	second := postWithHeader(t, ts, path, headers, body)
	requireOK(t, second)
	assert.Contains(t, string(second.body), `"reused":true`)
	transcript["session_id"] = "00000000-0000-4000-8000-000000000002"
	delete(transcript, "url")
	conflict := postWithHeader(t, ts, path, headers, body)
	assert.Equal(t, http.StatusConflict, conflict.status, string(conflict.body))
	// An unkeyed retry never replaces provenance on an already closed issue.
	requireOK(t, postWithHeader(t, ts, path, nil, body))
	requireOK(t, postWithHeader(t, ts, issueURL(projectID, issueID, "actions/reopen"), nil, map[string]any{"actor": "agent-one"}))
	headers["Idempotency-Key"] = "transcript-close-two"
	requireOK(t, postWithHeader(t, ts, path, headers, body))
	events, err := h.DB().EventsAfter(context.Background(), db.EventsAfterParams{ProjectID: projectID, Limit: 100})
	require.NoError(t, err)
	var ids []string
	for _, e := range events {
		if e.Type != "issue.closed" {
			continue
		}
		var p struct {
			Transcript struct {
				SessionID string `json:"session_id"`
			} `json:"transcript"`
		}
		require.NoError(t, json.Unmarshal([]byte(e.Payload), &p))
		ids = append(ids, p.Transcript.SessionID)
	}
	assert.Equal(t, []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}, ids)
}

func TestCloseTranscript_DoesNotReplaceEvidence(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	resp := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"), nil, map[string]any{
		"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.",
		"transcript": map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"},
	})
	assert.Equal(t, http.StatusBadRequest, resp.status)
	assert.Contains(t, string(resp.body), "evidence required")
}

func TestCloseTranscript_DryRunAndInvalidInputDoNotPersist(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "dry_run": true,
		"transcript": map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}}
	path := issueURL(projectID, issueID, "actions/close")
	requireOK(t, postWithHeader(t, ts, path, nil, body))
	body["dry_run"] = false
	for _, ref := range []map[string]any{
		{"agent": "codex", "session_id": "/private/chat.jsonl"},
		{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001", "url": "https://agentsview.example?token=secret"},
	} {
		body["transcript"] = ref
		response := postWithHeader(t, ts, path, nil, body)
		assert.Equal(t, http.StatusBadRequest, response.status, string(response.body))
		assert.NotContains(t, string(response.body), "/private")
		assert.NotContains(t, string(response.body), "token=secret")
	}
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
	events, err := h.DB().EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: projectID, Limit: 100})
	require.NoError(t, err)
	for _, e := range events {
		assert.NotEqual(t, "issue.closed", e.Type)
	}
}

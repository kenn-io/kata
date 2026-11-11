package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestCloseTranscript_PersistenceAndRetries(t *testing.T) {
	for _, tc := range []struct{ agent, id, otherID string }{
		{"codex", "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"},
		{"opencode", "ses_0123456789AbCd", "ses_0123456789EfGh"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			transcript := map[string]any{"agent": tc.agent, "session_id": tc.id}
			h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
			path := issueURL(projectID, issueID, "actions/close")
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
			// The link is a derived locator: a changed AgentsView base still replays.
			transcript["url"] = "https://agentsview.example/archive/sessions/" + tc.agent + "/" + tc.id
			relinked := postWithHeader(t, ts, path, headers, body)
			requireOK(t, relinked)
			assert.Contains(t, string(relinked.body), `"reused":true`)
			transcript["session_id"] = tc.otherID
			delete(transcript, "url")
			conflict := postWithHeader(t, ts, path, headers, body)
			assert.Equal(t, http.StatusConflict, conflict.status, string(conflict.body))
			// An unkeyed retry never replaces provenance on an already closed issue.
			requireOK(t, postWithHeader(t, ts, path, nil, body))
			requireOK(t, postWithHeader(t, ts, issueURL(projectID, issueID, "actions/reopen"), nil, map[string]any{"actor": "agent-one"}))
			headers["Idempotency-Key"] = "transcript-close-two"
			requireOK(t, postWithHeader(t, ts, path, headers, body))
			events, err := h.DB().EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: projectID, Limit: 100})
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
			assert.Equal(t, []string{tc.id, tc.otherID}, ids)
		})
	}
}

func TestCloseTranscript_PresenceConflict(t *testing.T) {
	_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(projectID, issueID, "actions/close")
	body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "retry_protocol": "close-v1"}
	ref := map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}
	headers := map[string]string{"Idempotency-Key": "transcript-presence"}
	requireOK(t, postWithHeader(t, ts, path, headers, body))
	body["transcript"] = ref
	response := postWithHeader(t, ts, path, headers, body)
	assert.Equal(t, http.StatusConflict, response.status, string(response.body))
}

func TestCloseTranscript_SelectionAndRetry(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(fmt.Sprintf("record=%t", record), func(t *testing.T) {
			_, ts, projectID, issueID := bootstrapProjectWithIssue(t)
			path := issueURL(projectID, issueID, "actions/close")
			candidate := map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}
			body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "retry_protocol": "close-v1", "transcript": candidate, "transcript_selection": map[string]any{"project_id": projectID, "record": record}}
			headers := map[string]string{"Idempotency-Key": "selection-retry"}
			first := postWithHeader(t, ts, path, headers, body)
			requireOK(t, first)
			var out api.MutationResponse
			require.NoError(t, json.Unmarshal(first.body, &out.Body))
			require.NotNil(t, out.Body.Event)
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(out.Body.Event.Payload), &payload))
			if record {
				assert.Equal(t, candidate, payload["transcript"])
			} else {
				assert.NotContains(t, payload, "transcript")
			}
			body["transcript_selection"] = map[string]any{"project_id": 0, "record": !record}
			replay := postWithHeader(t, ts, path, headers, body)
			requireOK(t, replay)
			assert.Contains(t, string(replay.body), `"reused":true`)
			candidate["agent"] = "claude"
			assert.Equal(t, http.StatusConflict, postWithHeader(t, ts, path, headers, body).status)
			delete(body, "transcript_selection")
			delete(body, "transcript")
			assert.Equal(t, http.StatusConflict, postWithHeader(t, ts, path, headers, body).status)
		})
	}
}

func TestCloseTranscript_SelectionRejectsMovedProject(t *testing.T) {
	h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
	body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "transcript": map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}, "transcript_selection": map[string]any{"project_id": projectID + 1, "record": false}}
	response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"), nil, body)
	assert.Equal(t, http.StatusConflict, response.status, string(response.body))
	assert.Contains(t, string(response.body), "issue_moved")
	issue, err := h.DB().IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
}

func TestCloseTranscript_SelectionValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		candidate any
		selection map[string]any
		status    int
	}{
		{"missing candidate for a recorded close", nil, map[string]any{"project_id": 0, "record": true}, http.StatusBadRequest},
		{"unknown target with URL", map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001", "url": "https://archive.example/session"}, map[string]any{"project_id": 0, "record": true}, http.StatusBadRequest},
		{"missing record", map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}, map[string]any{"project_id": 0}, http.StatusBadRequest},
		{"opt-out without candidate", nil, map[string]any{"project_id": 0, "record": false}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ts, projectID, issueID := bootstrapProjectWithIssue(t)
			body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "transcript_selection": tc.selection}
			if tc.candidate != nil {
				body["transcript"] = tc.candidate
			}
			response := postWithHeader(t, ts, issueURL(projectID, issueID, "actions/close"), nil, body)
			assert.Equal(t, tc.status, response.status, string(response.body))
			if tc.status == http.StatusOK {
				issue, err := h.DB().IssueByID(t.Context(), issueID)
				require.NoError(t, err)
				assert.Equal(t, "closed", issue.Status)
			}
		})
	}
}

func TestCloseTranscript_MoveBeforeGuardedWrite(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(fmt.Sprintf("record=%t", record), func(t *testing.T) {
			database := openTestDB(t)
			source, err := database.db.CreateProject(t.Context(), "source")
			require.NoError(t, err)
			target, err := database.db.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			issue, _, err := database.db.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "move race", Author: "agent-one"})
			require.NoError(t, err)
			store := &closeMoveRaceStore{Storage: database.db, toProjectID: target.ID, moveNext: true}
			ts := startTestServer(t, daemon.ServerConfig{DB: store, StartedAt: database.now})
			body := map[string]any{"actor": "agent-one", "reason": "done", "message": "Implemented the example behavior and ran the focused tests.", "evidence": []map[string]any{{"type": "test", "command": "go test ./internal/example"}}, "transcript": map[string]any{"agent": "codex", "session_id": "00000000-0000-4000-8000-000000000001"}, "transcript_selection": map[string]any{"project_id": source.ID, "record": record}}
			if !record {
				delete(body, "transcript")
			}
			response := postWithHeader(t, ts, issueURLRef(source.ID, issue.ShortID, "actions/close"), nil, body)
			assertAPIError(t, response.status, response.body, http.StatusConflict, "issue_moved")
			current, err := database.db.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			assert.Equal(t, "open", current.Status)
			events, err := database.db.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: target.ID, Limit: 100})
			require.NoError(t, err)
			for _, event := range events {
				assert.NotEqual(t, "issue.closed", event.Type)
			}
		})
	}
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

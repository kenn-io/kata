package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCommentEndpoint_AppendsAndEmitsEvent(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "agent", "body": "first comment"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"body":"first comment"`)
	assert.Contains(t, string(bs), `"type":"issue.commented"`)
}

func TestCommentEndpoint_IdempotencyReusesCommittedComment(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(pid, issueID, "comments")
	body := map[string]any{"actor": "agent", "body": "first comment"}
	headers := map[string]string{"Idempotency-Key": "comment-request-1"}

	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var created struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(first.body, &created))
	require.NotEmpty(t, created.Comment.UID)

	second := postWithHeader(t, ts, path, headers, body)
	requireOK(t, second)
	var reused struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
		Event   *struct{} `json:"event"`
		Changed bool      `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(second.body, &reused))
	assert.Equal(t, created.Comment.UID, reused.Comment.UID)
	assert.Nil(t, reused.Event)
	assert.False(t, reused.Changed)

	comments, err := h.DB().CommentsByIssue(context.Background(), issueID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestCommentEndpoint_IdempotencyRejectsDifferentBody(t *testing.T) {
	_, ts, pid, issueID := bootstrapProjectWithIssue(t)
	path := issueURL(pid, issueID, "comments")
	headers := map[string]string{"Idempotency-Key": "comment-request-1"}

	first := postWithHeader(t, ts, path, headers,
		map[string]any{"actor": "agent", "body": "first comment"})
	requireOK(t, first)
	second := postWithHeader(t, ts, path, headers,
		map[string]any{"actor": "agent", "body": "different comment"})

	assertAPIError(t, second.status, second.body, 409, "idempotency_mismatch")
}

func TestCommentEndpoint_EditsCommentAndEmitsEvent(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "agent", "body": "token=leaked"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	var created struct {
		Comment struct {
			UID string `json:"uid"`
		} `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(bs, &created))
	require.NotEmpty(t, created.Comment.UID)

	editResp, editBS := patchJSON(t, ts, issueURL(pid, num, "comments/"+created.Comment.UID),
		map[string]any{"actor": "redactor", "body": "[redacted]"})
	require.Equal(t, 200, editResp.StatusCode, string(editBS))
	assert.Contains(t, string(editBS), `"body":"[redacted]"`)
	assert.Contains(t, string(editBS), `"type":"issue.comment_edited"`)
	assert.NotContains(t, string(editBS), "token=leaked")
}

func TestCommentEndpoint_RejectsEditingExternallyOwnedComment(t *testing.T) {
	h, ts, pid, issueID := bootstrapProjectWithIssue(t)
	observedAt := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	binding, _, err := h.DB().CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: pid, IssueID: issueID, ConnectorInstance: "example-connector",
		ExternalRootKey: "external-root", ExternalAccountKey: "external-account",
		Actor: "connector:example-connector", ReceiveCommentsAfter: observedAt,
	})
	require.NoError(t, err)
	claimed, acquired, err := h.DB().ClaimExternalRootBinding(
		t.Context(), binding.ID, "projection-claim", observedAt, observedAt.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)
	comment, _, _, err := h.DB().UpsertExternalCommentProjection(t.Context(), db.ExternalCommentProjectionParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken,
		ExternalID: "external-comment", ExternalRevision: "revision-1",
		Body: "Provider-owned comment", ExternalActorID: "external-actor",
		ExternalActorName: "External author", ExternalCreatedAt: observedAt,
		ExternalUpdatedAt: observedAt, IntegrationActor: "connector:example-connector",
	})
	require.NoError(t, err)

	resp, body := patchJSON(t, ts, issueURL(pid, issueID, "comments/"+comment.UID),
		map[string]any{"actor": "redactor", "body": "local replacement"})

	assertAPIError(t, resp.StatusCode, body, 409, "external_comment_content_owned")
}

func TestActionsClose_ReopenRoundtrip(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{
			"actor":   "agent",
			"reason":  "wontfix",
			"message": "Decided not to fix this; out of scope for this milestone and not aligned with roadmap.",
		})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"status":"closed"`)
	assert.Contains(t, string(bs), `"closed_reason":"wontfix"`)

	resp2, bs2 := postJSON(t, ts, issueURL(pid, num, "actions/reopen"),
		map[string]any{"actor": "agent"})
	require.Equal(t, 200, resp2.StatusCode, string(bs2))
	assert.Contains(t, string(bs2), `"status":"open"`)
}

func TestActionsClose_RejectsUnsupportedReason(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{"actor": "agent", "reason": "obsolete"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestActionsClose_AlreadyClosedIsNoOpEnvelope(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)
	body := map[string]any{
		"actor":   "agent",
		"reason":  "wontfix",
		"message": "Decided not to fix this; out of scope for this milestone and not aligned with roadmap.",
	}
	_, _ = postJSON(t, ts, issueURL(pid, num, "actions/close"), body)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"), body)
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"changed":false`)
	assert.Contains(t, string(bs), `"event":null`)
}

func TestCreateComment_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "   ", "body": "hi"})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestCloseIssue_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/close"),
		map[string]any{"actor": "   "})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

func TestReopenIssue_BlankActorIs400(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "actions/reopen"),
		map[string]any{"actor": "   "})
	assertAPIError(t, resp.StatusCode, bs, 400, "validation")
}

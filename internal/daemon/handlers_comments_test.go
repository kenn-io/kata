package daemon_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCommentEndpoint_AppendsAndEmitsEvent(t *testing.T) {
	_, ts, pid, num := bootstrapProjectWithIssue(t)

	resp, bs := postJSON(t, ts, issueURL(pid, num, "comments"),
		map[string]any{"actor": "agent", "body": "first comment"})
	require.Equal(t, 200, resp.StatusCode, string(bs))
	assert.Contains(t, string(bs), `"body":"first comment"`)
	assert.Contains(t, string(bs), `"type":"issue.commented"`)
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

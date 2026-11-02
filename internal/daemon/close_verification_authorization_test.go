package daemon_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// H1: an exact receipt replay must not return the other issue's warning.
func TestCloseVerificationScopedRetryRedactsWarnings(t *testing.T) {
	env, project, source, outside, _ := scopedReplyFixture(t, true)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "owner-close-token", Actor: "worker", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	refute, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: outside.IssueID, Author: "hidden-reviewer", Body: "Hidden review excerpt must never cross the subtree boundary.", ReplyToUID: claim.UID, ReplyKind: "refute"})
	require.NoError(t, err)
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + source.ShortID + "/actions/close"
	body := map[string]any{"reason": "done", "message": "Completed and checked the requested change in the focused suite.", "retry_protocol": "close-v1", "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}}}
	headers := map[string]string{"Authorization": "Bearer owner-close-token", "Idempotency-Key": "warning-receipt"}
	resp, raw := envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "Hidden review excerpt")
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{IssueUID: source.UID, Types: []string{"issue.closed"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Contains(t, events[0].Payload, refute.UID, "retained references must be immutable UIDs")
	headers["Authorization"] = "Bearer worker-token"
	resp, raw = envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"reused":true`)
	require.NotContains(t, string(raw), "Hidden review excerpt")
	require.NotContains(t, string(raw), "hidden-reviewer")
	require.NotContains(t, string(raw), refute.UID)
}

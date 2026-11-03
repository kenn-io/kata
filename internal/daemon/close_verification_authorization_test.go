package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
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
	// Rendered payloads no longer match the canonical replication hash.
	var feed struct {
		Events []api.EventEnvelope `json:"events"`
	}
	resp, raw = envDoRaw(t, env, http.MethodGet, "/api/v1/events?after_id="+strconv.FormatInt(events[0].ID-1, 10), nil, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &feed))
	require.NotEmpty(t, events[0].ContentHash)
	require.NotEmpty(t, feed.Events)
	require.Equal(t, events[0].UID, feed.Events[0].EventUID)
	require.NotContains(t, string(feed.Events[0].Payload), refute.UID)
	require.Empty(t, feed.Events[0].ContentHash)
	headers["Authorization"] = "Bearer worker-token"
	resp, raw = envDoRaw(t, env, http.MethodPost, path, body, headers)
	require.Equal(t, 200, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), `"reused":true`)
	require.NotContains(t, string(raw), "Hidden review excerpt")
	require.NotContains(t, string(raw), "hidden-reviewer")
	require.NotContains(t, string(raw), refute.UID)
}

func TestCloseDryRunReturnsVerificationEndpointProjectProvenance(t *testing.T) {
	env, project, source, target, _ := scopedReplyFixture(t, true)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "The requested result is now correct.",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: target.IssueID, Author: "reviewer", Body: "The result still needs review.",
		ReplyToUID: claim.UID, ReplyKind: "refute",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "owner-close-token", Actor: "worker", AdminActor: db.BootstrapActor,
	})
	require.NoError(t, err)
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + source.ShortID + "/actions/close"
	body := map[string]any{
		"reason": "done", "message": "Completed and checked the requested change in the focused suite.",
		"dry_run": true, "evidence": []map[string]any{{"type": "test", "command": "go test ./example"}},
	}
	resp, raw := envDoRaw(t, env, http.MethodPost, path, body, map[string]string{
		"Authorization": "Bearer owner-close-token",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var response struct {
		Warnings []struct {
			Refutations []struct {
				Refutation string `json:"refutation"`
				Target     string `json:"target"`
			} `json:"refutations"`
		} `json:"warnings"`
		VerificationEndpointProjects map[string]string `json:"verification_endpoint_projects"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Len(t, response.Warnings, 1)
	require.Len(t, response.Warnings[0].Refutations, 1)
	refutation := response.Warnings[0].Refutations[0]
	require.Equal(t, project.UID, response.VerificationEndpointProjects[refutation.Refutation])
	require.Equal(t, project.UID, response.VerificationEndpointProjects[refutation.Target])
}

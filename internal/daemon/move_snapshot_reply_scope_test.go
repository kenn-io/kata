package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMovedSnapshotReplyProjectScope(t *testing.T) {
	for _, surface := range []string{"poll", "SSE"} {
		for _, visible := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/visible=%t", surface, visible), func(t *testing.T) {
				env := testenv.New(t, testenv.WithAuthToken("owner-token"))
				source, target, issue := seedMovePair(t, env)
				other, err := env.DB.CreateProject(t.Context(), "other-project")
				require.NoError(t, err)
				for _, project := range []db.Project{source, target, other} {
					_, err = env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
					require.NoError(t, err)
				}
				peer, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: other.ID, Title: "reply target", Author: "tester"})
				require.NoError(t, err)
				comment, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: peer.ID, Author: "tester", Body: "target comment"})
				require.NoError(t, err)
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "nested reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
				require.NoError(t, err)
				moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
				require.NoError(t, err)
				allowed := []string{source.UID, target.UID}
				if visible {
					allowed = append(allowed, other.UID)
				}
				scope := requestScopeValue(t, allowed...)
				if surface == "poll" {
					headers := bearer("owner-token")
					headers[requestProjectScopeHeader] = scope
					response, body := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", target.ID, moved.EventID-1), nil, headers)
					require.Equal(t, http.StatusOK, response.StatusCode, string(body))
					var out api.PollEventsBody
					require.NoError(t, json.Unmarshal(body, &out))
					if visible {
						require.False(t, out.ResetRequired)
						require.Contains(t, string(body), comment.UID)
						return
					}
					require.True(t, out.ResetRequired)
					require.Equal(t, moved.EventID, out.ResetAfterID)
					require.Empty(t, out.Events)
					require.NotContains(t, string(body), comment.UID)
					require.NotContains(t, string(body), `reply_kind`)
					return
				}
				response := openSSE(t, env, fmt.Sprintf("project_id=%d&after_id=%d", target.ID, moved.EventID-1), http.Header{"Authorization": {"Bearer owner-token"}, requestProjectScopeHeader: {scope}})
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				frame, ok := newSSEFramer(response.Body).Next(t, 2*time.Second)
				require.True(t, ok)
				require.Equal(t, strconv.FormatInt(moved.EventID, 10), frame.id)
				if visible {
					require.Equal(t, "issue.moved", frame.event)
					require.Contains(t, frame.data, comment.UID)
				} else {
					require.Equal(t, "sync.reset_required", frame.event)
					require.JSONEq(t, fmt.Sprintf(`{"event_id":%d,"reset_after_id":%d}`, moved.EventID, moved.EventID), frame.data)
					require.NotContains(t, frame.data, comment.UID)
					require.NotContains(t, frame.data, `reply_kind`)
				}
			})
		}
	}
}

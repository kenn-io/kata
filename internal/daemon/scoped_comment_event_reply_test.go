package daemon_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// A reply on a granted issue is an ordinary comment for a scoped reader. The
// reply's target issue may sit outside the subtree, so the event drops the
// target endpoint and reply fields instead of resetting the reader.
func TestIssueScopedReplyEventDeliveredWithoutTarget(t *testing.T) {
	for _, surface := range []string{"poll", "SSE", "snapshot history"} {
		t.Run(surface, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
			source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
			outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside target", nil)
			target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: outside.ID, Author: "coordinator", Body: "Target comment",
			})
			require.NoError(t, err)
			newScopedTokens(t, env, project, root)
			afterID, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			query := "after_id=" + strconv.FormatInt(afterID, 10)
			var framer *sseFramer
			if surface == "SSE" {
				stream := openSSE(t, env, query, http.Header{"Authorization": {"Bearer worker-token"}})
				t.Cleanup(func() { _ = stream.Body.Close() })
				framer = newSSEFramer(stream.Body)
			}

			reply, event, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: source.ID, Author: "worker", Body: "Reply comment",
				ReplyToUID: target.UID, ReplyKind: "reply",
			})
			require.NoError(t, err)
			require.NotNil(t, event.RelatedIssueID)
			require.Equal(t, outside.ID, *event.RelatedIssueID)
			env.Broadcaster.Broadcast(daemon.NewEventMsg(event.ProjectID, event))

			var delivered string
			switch surface {
			case "poll":
				resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/events?"+query, nil,
					map[string]string{"Authorization": "Bearer worker-token"})
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
				var polled api.PollEventsResponse
				require.NoError(t, json.Unmarshal(body, &polled.Body))
				require.False(t, polled.Body.ResetRequired)
				require.Len(t, polled.Body.Events, 1)
				require.Equal(t, "issue.commented", polled.Body.Events[0].Type)
				delivered = string(body)
			case "SSE":
				frame, ok := framer.Next(t, 2*time.Second)
				require.True(t, ok, "live scoped stream should deliver the source comment")
				require.Equal(t, "issue.commented", frame.event)
				delivered = frame.data
			default:
				path := "/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID + "&include_history=true"
				resp, body := envDoRaw(t, env, http.MethodGet, path, nil,
					map[string]string{"Authorization": "Bearer worker-token"})
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
				var snapshot struct {
					Selected struct {
						History []db.Event `json:"history"`
					} `json:"selected"`
				}
				require.NoError(t, json.Unmarshal(body, &snapshot))
				found := false
				for _, historyEvent := range snapshot.Selected.History {
					found = found || (historyEvent.Type == "issue.commented" && strings.Contains(historyEvent.Payload, reply.UID))
				}
				require.True(t, found, "snapshot history should keep the source comment")
				delivered = string(body)
			}
			require.Contains(t, delivered, reply.UID)
			require.NotContains(t, delivered, target.UID)
			require.NotContains(t, delivered, outside.UID)
			require.NotContains(t, delivered, `reply_to_uid`)
			require.NotContains(t, delivered, `reply_kind`)
		})
	}
}

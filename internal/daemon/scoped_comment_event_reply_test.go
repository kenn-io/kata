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

func TestIssueScopedPollAndSnapshotKeepAccessibleReplyTarget(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
	source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
	targetIssue := createScopedHTTPTestIssue(t, env, project.ID, "Allowed target", &root)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: targetIssue.ID, Author: "worker-a", Body: "Target comment",
	})
	require.NoError(t, err)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	reply, event, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker-a", Body: "Reply comment",
		ReplyToUID: target.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.NotNil(t, event.RelatedIssueID)
	require.Equal(t, targetIssue.ID, *event.RelatedIssueID)

	for _, path := range []string{
		"/api/v1/events?after_id=" + strconv.FormatInt(afterID, 10),
		"/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID + "&include_history=true",
	} {
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil,
			map[string]string{"Authorization": "Bearer worker-token"})
		require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
		require.Contains(t, string(body), reply.UID)
		require.Contains(t, string(body), target.UID)
		require.Contains(t, string(body), `"reply_to_uid"`)
		require.Contains(t, string(body), `"reply_kind":"reply"`)
	}
}

func TestIssueScopedPollAndSnapshotRedactCrossProjectReplyTarget(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
	source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
	targetIssue := createScopedHTTPTestIssue(t, env, otherProject.ID, "Cross-project target", nil)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: targetIssue.ID, Author: "coordinator", Body: "Target comment",
	})
	require.NoError(t, err)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	stream := openSSE(t, env, "after_id="+strconv.FormatInt(afterID, 10),
		http.Header{"Authorization": {"Bearer worker-token"}})
	defer func() { _ = stream.Body.Close() }()
	framer := newSSEFramer(stream.Body)

	// Storage can replay historical/imported relationships that new HTTP
	// mutations reject. The durable event has no related-issue envelope when
	// its reply target belongs to another project.
	reply, event, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker-a", Body: "Reply comment",
		ReplyToUID: target.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.Nil(t, event.RelatedIssueID)
	require.Nil(t, event.RelatedIssueUID)
	env.Broadcaster.Broadcast(daemon.NewEventMsg(event.ProjectID, event))

	frame, ok := framer.Next(t, 2*time.Second)
	require.True(t, ok, "live scoped stream should deliver the source comment")
	require.Equal(t, "issue.commented", frame.event)
	require.NotContains(t, frame.data, target.UID)
	require.NotContains(t, frame.data, `"reply_to_uid"`)
	require.NotContains(t, frame.data, `"reply_kind"`)
	assertScopedCommentEventReplyRedacted(t, env, source, reply, target.UID, afterID)
}

func TestIssueScopedCrossScopeReplyResetsPollAndSSE(t *testing.T) {
	for _, surface := range []string{"poll", "SSE"} {
		t.Run(surface, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
			source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
			outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside target", nil)
			target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: outside.ID, Author: "worker-a", Body: "Target comment",
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

			// The comment belongs to the allowed source issue, but its reply
			// target is outside the token subtree. The client must refresh rather
			// than silently advancing past the visible source comment.
			_, event, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: source.ID, Author: "worker-a", Body: "Reply comment",
				ReplyToUID: target.UID, ReplyKind: "reply",
			})
			require.NoError(t, err)
			require.NotNil(t, event.RelatedIssueID)
			require.Equal(t, outside.ID, *event.RelatedIssueID)
			env.Broadcaster.Broadcast(daemon.NewEventMsg(event.ProjectID, event))

			if surface == "poll" {
				resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/events?"+query, nil,
					map[string]string{"Authorization": "Bearer worker-token"})
				require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
				var polled api.PollEventsResponse
				require.NoError(t, json.Unmarshal(body, &polled.Body))
				require.True(t, polled.Body.ResetRequired)
				require.Equal(t, event.ID, polled.Body.ResetAfterID)
				require.Equal(t, event.ID, polled.Body.NextAfterID)
				require.Empty(t, polled.Body.Events)
				require.NotContains(t, string(body), target.UID)
				require.NotContains(t, string(body), outside.UID)
				return
			}

			frame, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok, "live scoped stream should reset for the hidden reply endpoint")
			require.Equal(t, "sync.reset_required", frame.event)
			var reset api.EventReset
			require.NoError(t, json.Unmarshal([]byte(frame.data), &reset))
			require.Equal(t, event.ID, reset.ResetAfterID)
			require.NotContains(t, frame.data, target.UID)
			require.NotContains(t, frame.data, outside.UID)
		})
	}
}

func TestIssueScopedPollAndSnapshotRecheckLateReplyTargetArrival(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
	source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside target", nil)
	newScopedTokens(t, env, project, root)
	afterID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	targetUID := "01EEEEEEEEEEEEEEEEEEEEEEEE"

	// The source event predates the target comment, as can happen when an
	// imported reply arrives before the project containing its target.
	reply, event, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker-a", Body: "Reply before target",
		ReplyToUID: targetUID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.Nil(t, event.RelatedIssueID)
	require.Nil(t, event.RelatedIssueUID)
	assertScopedCommentEventReplyRedacted(t, env, source, reply, targetUID, afterID)

	_, err = env.DB.ExecContext(t.Context(),
		`INSERT INTO comments(uid, issue_id, author, body) VALUES (?, ?, ?, ?)`,
		targetUID, outside.ID, "coordinator", "Late target comment")
	require.NoError(t, err)
	targetIssues, err := env.DB.CommentIssueIDsByUIDs(t.Context(), []string{targetUID})
	require.NoError(t, err)
	require.Equal(t, outside.ID, targetIssues[targetUID])

	// Re-read the same durable event after the target exists. Projection must
	// recheck current ownership and keep the outside target hidden.
	assertScopedCommentEventReplyRedacted(t, env, source, reply, targetUID, afterID)
}

func assertScopedCommentEventReplyRedacted(
	t *testing.T,
	env *testenv.Env,
	source db.Issue,
	reply db.Comment,
	targetUID string,
	afterID int64,
) {
	t.Helper()
	paths := []struct {
		name    string
		path    string
		history bool
	}{
		{name: "poll", path: "/api/v1/events?after_id=" + strconv.FormatInt(afterID, 10)},
		{
			name:    "snapshot history",
			path:    "/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID + "&include_history=true",
			history: true,
		},
	}
	for _, test := range paths {
		t.Run(test.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodGet, test.path, nil,
				map[string]string{"Authorization": "Bearer worker-token"})
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
			require.NotContains(t, string(body), targetUID,
				"the reply target UID must not reach a scoped caller")
			var payload json.RawMessage
			if test.history {
				var snapshot struct {
					Selected struct {
						History []db.Event `json:"history"`
					} `json:"selected"`
				}
				require.NoError(t, json.Unmarshal(body, &snapshot))
				for _, event := range snapshot.Selected.History {
					if event.Type == "issue.commented" && strings.Contains(event.Payload, reply.UID) {
						payload = json.RawMessage(event.Payload)
						break
					}
				}
			} else {
				var polled struct {
					Events []scopedEnvelope `json:"events"`
				}
				require.NoError(t, json.Unmarshal(body, &polled))
				for _, event := range polled.Events {
					if event.Type == "issue.commented" && strings.Contains(string(event.Payload), reply.UID) {
						payload = event.Payload
						break
					}
				}
			}
			require.NotEmpty(t, payload, "scoped event projection should retain the source comment")
			require.Contains(t, string(payload), reply.UID)
			require.NotContains(t, string(payload), `"reply_to_uid"`)
			require.NotContains(t, string(payload), `"reply_kind"`)
		})
	}
}

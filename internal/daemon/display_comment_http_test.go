package daemon_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/teammate"
)

func TestCommentDisplayContributionHTTPKeepsRouting(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	target, _, err := h.DB().CreateComment(teammate.WithRequest(t.Context(), "target-display"), db.CreateCommentParams{IssueID: id, Author: "finder", Teammate: "review", Body: "Finding"})
	require.NoError(t, err)
	path := issueURL(pid, id, "comments")
	body := map[string]any{"actor": "worker", "teammate": "route", "body": "Response", "reply_to": target.UID, "kind": "reply"}
	headers := map[string]string{"X-Kata-Teammate": "writer", "Idempotency-Key": "display-reply"}
	first := postWithHeader(t, ts, path, headers, body)
	requireOK(t, first)
	var created struct {
		Comment db.Comment
		Event   db.Event
	}
	require.NoError(t, json.Unmarshal(first.body, &created))
	require.NotNil(t, created.Comment.AttributedTeammate)
	require.Equal(t, "writer", *created.Comment.AttributedTeammate)
	require.Equal(t, "route", created.Comment.Teammate)
	requireOK(t, postWithHeader(t, ts, path, headers, body))
	duplicate := postWithHeader(t, ts, path, map[string]string{"X-Kata-Teammate": "another-display"}, body)
	require.Equal(t, 409, duplicate.status, string(duplicate.body))
	require.Contains(t, string(duplicate.body), "duplicate_reply")

	current, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	var slots map[string]any
	require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
	require.Contains(t, slots, notification.MetadataKey("finder/review"))
	require.NotContains(t, slots, notification.MetadataKey("finder/target-display"))

	for _, tc := range []struct {
		inbound string
		count   int
	}{{"finder/review", 1}, {"finder/target-display", 0}} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+issueURL(pid, id, "")+"?inbound="+tc.inbound, nil)
		require.NoError(t, err)
		response, err := ts.Client().Do(request)
		require.NoError(t, err)
		var shown struct{ Comments []commentref.Record }
		require.NoError(t, json.NewDecoder(response.Body).Decode(&shown))
		require.NoError(t, response.Body.Close())
		require.Equal(t, 200, response.StatusCode)
		require.Len(t, shown.Comments, tc.count)
		if tc.count > 0 {
			require.Equal(t, "writer", *shown.Comments[0].AttributedTeammate)
		}
	}
	response, encoded := patchJSON(t, ts, path+"/"+created.Comment.UID, map[string]any{"actor": "editor", "body": "Revised response"})
	require.Equal(t, 200, response.StatusCode, string(encoded))
	var edited struct{ Comment db.Comment }
	require.NoError(t, json.Unmarshal(encoded, &edited))
	require.Equal(t, "writer", *edited.Comment.AttributedTeammate)
	require.Equal(t, "route", edited.Comment.Teammate)
}

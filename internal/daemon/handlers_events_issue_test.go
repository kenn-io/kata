package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// The issue history contract filters before limit and acknowledges the resolved
// subject even on the final empty page, so clients can reject older daemons.
func TestPollProjectEventsIssuePagination(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	mkIssue(t, env, project.ID, "Unrelated before")
	issue, created := mkIssueWithEvent(t, env, project.ID, "Subject")
	mkIssue(t, env, project.ID, "Unrelated between")
	_, commented, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "Progress"})
	require.NoError(t, err)
	for _, ref := range []string{issue.ShortID, issue.UID, project.Name + "#" + issue.ShortID} {
		after := int64(0)
		for _, want := range []int64{created.ID, commented.ID, 0} {
			resp, body := envGetRaw(t, env, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), url.QueryEscape(ref), after))
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			require.Equal(t, issue.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
			var page api.PollEventsBody
			require.NoError(t, json.Unmarshal(body, &page))
			require.False(t, page.ResetRequired)
			if want == 0 {
				require.Empty(t, page.Events)
				require.Equal(t, after, page.NextAfterID)
				continue
			}
			require.Len(t, page.Events, 1)
			require.Equal(t, want, page.Events[0].EventID)
			require.Equal(t, issue.UID, *page.Events[0].IssueUID)
			require.Equal(t, want, page.NextAfterID)
			after = page.NextAfterID
		}
	}
	other, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreign := mkIssue(t, env, other.ID, "Foreign")
	for _, ref := range []string{"invalid!", foreign.UID, other.Name + "#" + foreign.ShortID} {
		resp, body := envGetRaw(t, env, scopedProjectPath(project.ID, "events")+"?issue="+url.QueryEscape(ref))
		require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	}
	for _, suffix := range []string{"&limit=0", "&after_id=-1"} {
		resp, _ := envGetRaw(t, env, scopedProjectPath(project.ID, "events")+"?issue="+issue.UID+suffix)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}
	_, err = env.DB.PurgeIssue(t.Context(), foreign.ID, "tester", nil)
	require.NoError(t, err)
	// Purging another issue in this project leaves the subject's history
	// readable, both fresh and from a cursor held across the purge.
	doomed := mkIssue(t, env, project.ID, "Doomed")
	_, err = env.DB.PurgeIssue(t.Context(), doomed.ID, "tester", nil)
	require.NoError(t, err)
	for after, want := range map[int64][]int64{0: {created.ID, commented.ID}, created.ID: {commented.ID}} {
		resp, body := envGetRaw(t, env, fmt.Sprintf("%s?issue=%s&after_id=%d", scopedProjectPath(project.ID, "events"), issue.UID, after))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.False(t, page.ResetRequired)
		got := make([]int64, 0, len(page.Events))
		for _, event := range page.Events {
			got = append(got, event.EventID)
		}
		require.Equal(t, want, got)
	}
}

func TestPollProjectEventsIssueScopedHistory(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	hidden := createScopedHTTPTestIssue(t, env, project.ID, "Hidden", nil)
	other := createScopedHTTPTestIssue(t, env, project.ID, "Other hidden peer", nil)
	newScopedTokens(t, env, project, root)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	resp, body := envDoRaw(t, env, http.MethodGet, scopedProjectPath(project.ID, "events")+"?issue="+hidden.UID, nil, headers)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	// Each hidden-peer link is omitted from history. A visible comment after
	// multiple omitted pages must still fill this limit=1 response.
	for range 2 {
		peer := createScopedHTTPTestIssue(t, env, project.ID, "Hidden peer", nil)
		resp, body = envDoRaw(t, env, http.MethodPost, scopedProjectPath(project.ID, "issues/"+root.ShortID+"/links"),
			map[string]any{"actor": "coordinator", "type": "blocks", "to_ref": peer.UID}, map[string]string{"Authorization": "Bearer coordinator-token"})
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	}
	_, comment, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: root.ID, Author: "tester", Body: "Visible progress"})
	require.NoError(t, err)
	resp, body = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), root.UID, after), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, root.UID, resp.Header.Get("X-Kata-Events-Issue-UID"))
	var page api.PollEventsBody
	require.NoError(t, json.Unmarshal(body, &page))
	require.False(t, page.ResetRequired)
	require.Len(t, page.Events, 1)
	require.Equal(t, comment.ID, page.Events[0].EventID)
	require.Empty(t, page.Events[0].OriginInstanceUID)
	require.Empty(t, page.Events[0].ContentHash)
	// Compound link history survives with its peer-bearing payload redacted.
	resp, body = envDoRaw(t, env, http.MethodPatch, scopedProjectPath(project.ID, "issues/"+root.ShortID),
		map[string]any{"actor": "coordinator", "links_delta": map[string]any{"add_related": []string{hidden.UID, other.UID}}},
		map[string]string{"Authorization": "Bearer coordinator-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), "issue.links_changed")
	resp, body = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d&limit=1", scopedProjectPath(project.ID, "events"), root.UID, comment.ID), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	page = api.PollEventsBody{}
	require.NoError(t, json.Unmarshal(body, &page))
	require.False(t, page.ResetRequired)
	require.Len(t, page.Events, 1, string(body))
	require.Equal(t, "issue.links_changed", page.Events[0].Type)
	require.Empty(t, page.Events[0].Payload)
	require.NotContains(t, string(body), hidden.UID)
}

func TestPollProjectEventsIssueOmitsPrivatePeerEvents(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: f.visible.ID, ToIssueID: f.issue.ID, Type: "parent", Author: "member",
		})
		require.NoError(t, err)
		after, err := store.MaxEventID(t.Context())
		require.NoError(t, err)
		_, events, _, err := store.CloseIssueWithEvents(t.Context(), f.visible.ID, "done", "member", "Completed visible task", nil)
		require.NoError(t, err)
		require.Len(t, events, 1)
		_, comment, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: f.visible.ID, Author: "member", Body: "Visible follow-up"})
		require.NoError(t, err)
		// History omits the event that names a private peer instead of
		// truncating the read at it.
		status, headers, body := f.request(t, http.MethodGet, fmt.Sprintf("%s?issue=%s&after_id=%d", scopedProjectPath(f.public.ID, "events"), f.visible.UID, after), "nonmember", nil, nil)
		require.Equal(t, http.StatusOK, status, string(body))
		require.Equal(t, f.visible.UID, headers.Get("X-Kata-Events-Issue-UID"))
		var page api.PollEventsBody
		require.NoError(t, json.Unmarshal(body, &page))
		require.False(t, page.ResetRequired)
		require.Len(t, page.Events, 1, string(body))
		require.Equal(t, comment.ID, page.Events[0].EventID)
		require.Equal(t, comment.ID, page.NextAfterID)
		require.NotContains(t, string(body), f.issue.UID)
	})
}

package daemon_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
)

func TestTypedCommentWriteAndDuplicate(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	path := issueURL(pid, id, "comments")
	body := map[string]any{"actor": "worker", "body": "Answer", "reply_to": "c:" + strings.ToLower(target.UID[20:]), "kind": "reply", "teammate": "reviewer"}
	first := postWithHeader(t, ts, path, nil, body)
	requireOK(t, first)
	var result struct{ Comment db.Comment }
	require.NoError(t, json.Unmarshal(first.body, &result))
	require.Equal(t, target.UID, result.Comment.ReplyToUID)
	require.Equal(t, "reply", result.Comment.ReplyKind)
	duplicate := postWithHeader(t, ts, path, nil, body)
	require.Equal(t, 409, duplicate.status, string(duplicate.body))
	require.Contains(t, string(duplicate.body), "duplicate_reply")
	require.Contains(t, string(duplicate.body), "c:")
	var failure struct{ Error struct{ Message string } }
	require.NoError(t, json.Unmarshal(duplicate.body, &failure))
	require.Contains(t, failure.Error.Message, "c:", "clients must be able to show the visible existing reply handle in the error")
	body["force"] = true
	requireOK(t, postWithHeader(t, ts, path, nil, body))
	body["force"] = false
	body["kind"] = "confirm"
	body["body"] = strings.Repeat("é", 39)
	require.Equal(t, 400, postWithHeader(t, ts, path, nil, body).status)
	body["body"] = strings.Repeat("é", 40)
	requireOK(t, postWithHeader(t, ts, path, nil, body))
}

func TestTypedCommentTargetsAndReceipts(t *testing.T) {
	for _, form := range []string{"full", "issue", "project"} {
		t.Run(form, func(t *testing.T) {
			h, ts, pid, id := bootstrapProjectWithIssue(t)
			p, err := h.DB().ProjectByID(t.Context(), pid)
			require.NoError(t, err)
			targetIssue, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Author: "finder", Title: "Finding task"})
			require.NoError(t, err)
			target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: targetIssue.ID, Author: "finder", Body: "Finding"})
			require.NoError(t, err)
			ref := target.UID
			if form != "full" {
				ref = targetIssue.ShortID + ":" + strings.ToLower(target.UID[20:])
			}
			if form == "project" {
				ref = p.Name + "#" + ref
			}
			body := map[string]any{"actor": "worker", "body": "Answer", "reply_to": ref, "kind": "reply"}
			headers := map[string]string{"Idempotency-Key": "typed-reply"}
			path := issueURL(pid, id, "comments")
			first := postWithHeader(t, ts, path, headers, body)
			requireOK(t, first)
			require.Contains(t, string(first.body), target.UID)
			_, err = h.DB().PurgeIssue(t.Context(), targetIssue.ID, "worker", nil)
			require.NoError(t, err)
			retry := postWithHeader(t, ts, path, headers, body)
			requireOK(t, retry)
			require.Contains(t, string(retry.body), `"changed":false`)
			body["reply_to"] = strings.ToLower(target.UID)
			requireOK(t, postWithHeader(t, ts, path, headers, body))
			body["kind"] = "supersede"
			require.Equal(t, 409, postWithHeader(t, ts, path, headers, body).status)
		})
	}
}

func TestCommentShowGraphSelectors(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	other, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Author: "worker", Title: "Reply task"})
	require.NoError(t, err)
	reply, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: "Answer", ReplyToUID: target.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	get := func(query string) struct {
		Comments          []commentref.Record
		CommentsTruncated bool `json:"comments_truncated"`
	} {
		resp, err := ts.Client().Get(fmt.Sprintf("%s/api/v1/projects/%d/issues/%s%s", ts.URL, pid, issue.ShortID, query))
		require.NoError(t, err)
		defer func() { require.NoError(t, resp.Body.Close()) }()
		require.Equal(t, 200, resp.StatusCode)
		var out struct {
			Comments          []commentref.Record
			CommentsTruncated bool `json:"comments_truncated"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return out
	}
	shown := get("")
	require.Len(t, shown.Comments, 1)
	require.NotEmpty(t, shown.Comments[0].Handle)
	require.Len(t, shown.Comments[0].Backlinks, 1)
	require.Equal(t, reply.UID, shown.Comments[0].Backlinks[0].UID)
	thread := get("?thread=" + target.UID)
	require.Len(t, thread.Comments, 2)
	require.Equal(t, reply.UID, thread.Comments[1].UID)
	inbound := get("?inbound=finder&kind=reply")
	require.Len(t, inbound.Comments, 1)
	require.Equal(t, reply.UID, inbound.Comments[0].UID)
	since := get("?thread=" + target.UID + "&since=" + target.UID)
	require.Len(t, since.Comments, 1)
	require.Equal(t, reply.UID, since.Comments[0].UID)
}

func TestCommentHandleEdit(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	c, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Before"})
	require.NoError(t, err)
	response, body := patchJSON(t, ts, issueURL(pid, id, "comments/c:"+strings.ToLower(c.UID[20:])), map[string]any{"actor": "worker", "body": "After"})
	require.Equal(t, 200, response.StatusCode, string(body))
	require.Contains(t, string(body), "edited_at")
}

func TestDeletedIssueCommentSelectors(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	root, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	reply, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "worker", Body: "Answer", ReplyToUID: root.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, _, _, err = h.DB().SoftDeleteIssue(t.Context(), id, "worker")
	require.NoError(t, err)
	resp, err := ts.Client().Get(fmt.Sprintf("%s/api/v1/projects/%d/issues/%s?include_deleted=true&thread=%s&since=%s", ts.URL, pid, issue.ShortID, root.UID, root.UID))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, 200, resp.StatusCode)
	var out struct{ Comments []commentref.Record }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Len(t, out.Comments, 1)
	require.Equal(t, reply.UID, out.Comments[0].UID)
}

func TestDeletedSourceCommentRetainsReplyEndpointState(t *testing.T) {
	for _, test := range []struct {
		name        string
		targetState string
		wantStatus  string
		wantProject string
	}{
		{name: "moved target", targetState: "moved", wantStatus: "moved", wantProject: "moved-project"},
		{name: "removed target", targetState: "removed", wantStatus: "removed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ts, projectID, sourceID := bootstrapProjectWithIssue(t)
			sourceIssue, err := h.DB().IssueByID(t.Context(), sourceID)
			require.NoError(t, err)
			targetIssue, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: projectID, Title: "Target issue", Author: "finder",
			})
			require.NoError(t, err)
			target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: targetIssue.ID, Author: "finder", Body: "Finding",
			})
			require.NoError(t, err)
			reply, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: sourceIssue.ID, Author: "worker", Body: "Response",
				ReplyToUID: target.UID, ReplyKind: "confirm",
			})
			require.NoError(t, err)

			if test.targetState == "moved" {
				movedProject, err := h.DB().CreateProject(t.Context(), test.wantProject)
				require.NoError(t, err)
				_, err = h.DB().MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
					IssueID: targetIssue.ID, FromProjectID: projectID, ToProjectID: movedProject.ID,
					IfMatchRev: targetIssue.Revision, Actor: "coordinator",
				})
				require.NoError(t, err)
			} else {
				_, _, _, err = h.DB().SoftDeleteIssue(t.Context(), targetIssue.ID, "coordinator")
				require.NoError(t, err)
			}
			_, _, _, err = h.DB().SoftDeleteIssue(t.Context(), sourceIssue.ID, "coordinator")
			require.NoError(t, err)

			path := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s?include_deleted=true",
				ts.URL, projectID, sourceIssue.ShortID)
			response, err := ts.Client().Get(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.Equal(t, 200, response.StatusCode)
			var out struct {
				Comments []commentref.Record `json:"comments"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&out))
			var got *commentref.Record
			for i := range out.Comments {
				if out.Comments[i].UID == reply.UID {
					got = &out.Comments[i]
					break
				}
			}
			require.NotNil(t, got)
			require.NotNil(t, got.Reply)
			require.Equal(t, target.UID, got.Reply.UID)
			require.Equal(t, test.wantStatus, got.Reply.Status)
			if test.wantProject != "" {
				require.Contains(t, got.Reply.Handle, test.wantProject+"#")
				require.NotContains(t, got.Reply.Handle, "pending")
			}
		})
	}
}

func TestCanonicalReplyReceiptAfterProjectRename(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	issue, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	path := issueURL(pid, id, "comments")
	headers := map[string]string{"Idempotency-Key": "canonical-reply"}
	body := map[string]any{"actor": "worker", "body": "Answer", "reply_to": target.UID, "kind": "reply"}
	requireOK(t, postWithHeader(t, ts, path, headers, body))
	renamed, err := h.DB().RenameProject(t.Context(), pid, "renamed-project")
	require.NoError(t, err)
	body["reply_to"] = renamed.Name + "#" + issue.ShortID + ":" + strings.ToLower(target.UID[20:])
	requireOK(t, postWithHeader(t, ts, path, headers, body))
}

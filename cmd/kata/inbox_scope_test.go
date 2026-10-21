package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestInboxBroadcastRetainsPointer(t *testing.T) {
	request := inboxRequest{From: "coordinator", Message: "Please inspect this comment", Re: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Broadcast: true}
	got := inboxAttentionText(request)
	if !strings.Contains(got, request.Re) {
		t.Fatalf("broadcast comment pointer absent: %s", got)
	}
}

func TestInboxRefreshesLaterHandleCollision(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Finding", Author: "coordinator"})
	require.NoError(t, err)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Body: "Original finding"})
	require.NoError(t, err)
	_, err = runCLICapture(t, env, dir, "--as", "worker", "comment", issue.ShortID, "--refute", target.UID, "--body", "The finding does not reproduce under independent verification.")
	require.NoError(t, err)
	comments, err := env.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	var reply db.Comment
	for _, c := range comments {
		if c.UID != target.UID {
			reply = c
		}
	}
	later, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "other-reader", Body: "Later unrelated comment"})
	require.NoError(t, err)
	collision := []byte(reply.UID)
	pos := len(collision) - 7
	if collision[pos] == '0' {
		collision[pos] = '1'
	} else {
		collision[pos] = '0'
	}
	_, err = env.DB.ExecContext(t.Context(), "UPDATE comments SET uid=? WHERE id=?", string(collision), later.ID)
	require.NoError(t, err)
	out, err := runCLICapture(t, env, dir, "inbox", "--for", "reader")
	require.NoError(t, err)
	t.Log(out)
	require.Contains(t, out, "c:"+strings.ToLower(reply.UID[len(reply.UID)-7:]), "inbox handles must remain unambiguous when later comments collide")
}

func TestInboxRefreshFailurePreservesOtherRequests(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			const uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/projects/resolve":
					_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
				case "/api/v1/projects/1/issues":
					_, _ = fmt.Fprintf(w, `{"issues":[{"project_id":1,"short_id":"abcd","title":"Finding","metadata":{"notify.cmVhZGVy":{"from":"worker","message":"latest: reply c:old by worker","kind":"reply","re":%q}}},{"project_id":1,"short_id":"efgh","title":"Other request","metadata":{"notify.cmVhZGVy":{"from":"worker","message":"Inspect another finding"}}}]}`, uid)
				default:
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"Refresh unavailable"}}`))
				}
			}))
			t.Cleanup(server.Close)
			resetFlags(t)
			stdout, stderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--workspace", t.TempDir(), "--project", "example-project", "inbox", "--for", "reader")
			require.NoError(t, err)
			require.Contains(t, stdout, uid)
			require.Contains(t, stdout, "Inspect another finding")
			require.NotContains(t, stdout, "c:old")
			require.Contains(t, stderr, "could not refresh notification")
		})
	}
}

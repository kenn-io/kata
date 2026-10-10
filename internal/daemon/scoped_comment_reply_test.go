package daemon_test

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Scoped callers do not see reply links yet, whether or not the target is in
// their subtree. Authorized reply rendering is a separate change.
func TestIssueScopedCommentResponsesOmitReplyFields(t *testing.T) {
	for _, targetOutsideSubtree := range []bool{true, false} {
		t.Run(fmt.Sprintf("target_outside=%t", targetOutsideSubtree), func(t *testing.T) {
			env, project, source, targetComment, replyComment := scopedReplyFixture(t, targetOutsideSubtree)
			base := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + source.ShortID
			headers := map[string]string{"Authorization": "Bearer worker-token"}
			tests := []struct {
				name   string
				method string
				path   string
				body   any
			}{
				{name: "show", method: http.MethodGet, path: base},
				{name: "snapshot", method: http.MethodGet,
					path: "/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID},
				{name: "comment edit", method: http.MethodPatch, path: base + "/comments/" + replyComment.UID,
					body: map[string]string{"actor": "worker", "body": "Updated reply"}},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					resp, body := envDoRaw(t, env, test.method, test.path, test.body, headers)
					require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
					require.Contains(t, string(body), replyComment.UID)
					require.NotContains(t, string(body), targetComment.UID)
					require.NotContains(t, string(body), `"reply_to_uid"`)
					require.NotContains(t, string(body), `"reply_kind"`)
				})
			}
		})
	}
}

func TestUnscopedShowKeepsReplyFields(t *testing.T) {
	env, project, source, targetComment, _ := scopedReplyFixture(t, true)
	path := "/api/v1/projects/" + strconv.FormatInt(project.ID, 10) + "/issues/" + source.ShortID
	resp, body := envDoRaw(t, env, http.MethodGet, path, nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.Contains(t, string(body), `"reply_to_uid":"`+targetComment.UID+`"`)
	require.Contains(t, string(body), `"reply_kind":"reply"`)
}

func scopedReplyFixture(t *testing.T, targetOutsideSubtree bool) (
	*testenv.Env, db.Project, db.Issue, db.Comment, db.Comment,
) {
	t.Helper()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Allowed root", nil)
	source := createScopedHTTPTestIssue(t, env, project.ID, "Reply source", &root)
	var target db.Issue
	if targetOutsideSubtree {
		target = createScopedHTTPTestIssue(t, env, project.ID, "Outside target", nil)
	} else {
		target = createScopedHTTPTestIssue(t, env, project.ID, "Inside target", &root)
	}
	targetComment, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: target.ID, Author: "coordinator", Body: "Target comment",
	})
	require.NoError(t, err)
	replyComment, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Reply comment",
		ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	return env, project, source, targetComment, replyComment
}

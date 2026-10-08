package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// A keyed request cannot change either endpoint or kind while keeping its
// original receipt; every field is independently framed by JSON encoding.
func FuzzTypedCommentFingerprint(f *testing.F) {
	f.Add("body", "reviewer", "01AAAAAAAAAAAAAAAAAAAAAAAA", "reply")
	f.Add("body\x00author", "", "01BBBBBBBBBBBBBBBBBBBBBBBB", "confirm")
	f.Fuzz(func(t *testing.T, body, tm, target, kind string) {
		// HTTP JSON input is valid UTF-8; canonical targets/kinds are ASCII.
		if !utf8.ValidString(body) || !utf8.ValidString(tm) || !utf8.ValidString(target) || !utf8.ValidString(kind) {
			return
		}
		a := commentIdempotencyFingerprint("issue", "worker", body, tm, target, kind)
		b := commentIdempotencyFingerprint("issue", "worker", body, tm, target+"X", kind)
		c := commentIdempotencyFingerprint("issue", "worker", body, tm, target, kind+"X")
		if a == b || a == c {
			t.Fatal("changed reply identity reused fingerprint")
		}
	})
}

func TestTypedReplyTargetMembershipFence(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	source := createScopedAuthIssue(t, store, project.ID, "Source", &root)
	target := createScopedAuthIssue(t, store, project.ID, "Target", &root)
	finding, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: target.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	handler := withScopedPrincipalRevalidation(store, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.NoError(t, authorizeIssueScopedIssue(r.Context(), store, source))
		require.NoError(t, authorizeIssueScopedIssue(r.Context(), store, target))
		parent, err := store.ParentOf(t.Context(), target.ID)
		require.NoError(t, err)
		require.NoError(t, store.DeleteLinkByID(t.Context(), parent.ID))
		_, _, err = store.CreateComment(r.Context(), db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Answer", ReplyToUID: finding.UID, ReplyKind: "reply", ValidateReply: true})
		require.Error(t, err)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	comments, err := store.CommentsByIssue(t.Context(), source.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// peerFencedHostAccess admits every request, but its transaction fence for
// any decision covering fencedProjectID writes inside the transaction and then
// rejects it with host cleanup: the host revoked that project after admission.
type peerFencedHostAccess struct {
	fencedProjectID int64
	fenceWrite      func(context.Context, db.Transaction) error
	finished        *atomic.Int32
	finishErr       *error
}

func (a peerFencedHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	fenced := slices.Contains(request.Operation.ProjectIDs, a.fencedProjectID)
	return daemon.HostAccessDecision{
		Revalidate: func(context.Context) error { return nil },
		TransactionFence: func(ctx context.Context, tx db.Transaction) error {
			if !fenced {
				return nil
			}
			if err := a.fenceWrite(ctx, tx); err != nil {
				return err
			}
			return db.AfterTransactionRollback(daemon.ErrHostAccessDenied, func(context.Context) error {
				a.finished.Add(1)
				return *a.finishErr
			})
		},
	}, nil
}

// A host admits a peer project for discovery with a decision whose
// transaction fence is the authority inside the write. When that fence
// rejects, the whole write rolls back, including the fence's own writes, and
// the host's cleanup runs once the transaction is gone.
func TestNotificationDiscoveryAppliesPeerHostTransactionFence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		var fenceTarget int64
		var finished atomic.Int32
		var finishErr error
		access := peerFencedHostAccess{fencedProjectID: f.private.ID, finished: &finished, finishErr: &finishErr,
			fenceWrite: func(ctx context.Context, tx db.Transaction) error {
				_, err := tx.ExecContext(ctx, `UPDATE issues SET title='fence wrote' WHERE id=$1`, fenceTarget)
				return err
			}}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: access})
		t.Cleanup(func() { _ = server.Close() })
		f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "member", Actor: "member"}))
			server.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(f.server.Close)
		requireRolledBack := func(t *testing.T, status int, body []byte, issueID int64, title string) {
			t.Helper()
			want := http.StatusNotFound
			if finishErr != nil {
				// A failed cleanup is retryable, not an ordinary denial.
				want = http.StatusServiceUnavailable
				require.Contains(t, string(body), "access_unavailable")
			}
			require.Equal(t, want, status, string(body))
			current, err := store.IssueByID(ctx, issueID)
			require.NoError(t, err)
			require.Equal(t, title, current.Title, "the rejecting fence's write must roll back")
			require.NotContains(t, string(current.Metadata), notification.KeyPrefix, "nothing from the rejected write commits")
			require.Equal(t, int32(1), finished.Load(), "host cleanup runs once after rollback")
		}

		for _, cleanup := range []error{nil, errors.New("host cleanup failed")} {
			finishErr = cleanup
			name := "broadcast open child"
			if cleanup != nil {
				name += " with failed cleanup"
			}
			t.Run(name, func(t *testing.T) {
				finished.Store(0)
				parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared parent", Author: "member"})
				require.NoError(t, err)
				child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Fenced child", Author: "worker", Owner: new("fenced-owner")})
				require.NoError(t, err)
				_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
				require.NoError(t, err)
				_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: parent.ID, Author: "public-reader", Body: "Watching"})
				require.NoError(t, err)
				fenceTarget = parent.ID

				status, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", f.public.ID, parent.ShortID),
					"member", map[string]any{"broadcast": true, "message": "Review the latest findings"}, nil)
				requireRolledBack(t, status, body, parent.ID, parent.Title)
			})
		}
		finishErr = nil

		t.Run("confirm with fenced parent", func(t *testing.T) {
			finished.Store(0)
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Fenced parent", Author: "worker", Owner: new("fenced-parent-owner")})
			require.NoError(t, err)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared child", Author: "member"})
			require.NoError(t, err)
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
			require.NoError(t, err)
			target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Author: "reviewer", Body: "Finding"})
			require.NoError(t, err)
			before, err := store.CommentsByIssue(ctx, child.ID)
			require.NoError(t, err)
			fenceTarget = child.ID

			status, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.public.ID, child.ShortID),
				"member", map[string]any{"actor": "member", "body": "Reproduced with the exact steps from the finding above.", "reply_to": target.UID, "kind": "confirm"}, nil)
			requireRolledBack(t, status, body, child.ID, child.Title)
			after, err := store.CommentsByIssue(ctx, child.ID)
			require.NoError(t, err)
			require.Len(t, after, len(before), "the rejected reply does not commit")
		})
	})
}

func TestNonFanoutCommentRepliesSkipParentHostFence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		var fenceTarget int64
		var finished atomic.Int32
		var finishErr error
		access := peerFencedHostAccess{fencedProjectID: f.private.ID, finished: &finished, finishErr: &finishErr,
			fenceWrite: func(ctx context.Context, tx db.Transaction) error {
				_, err := tx.ExecContext(ctx, `UPDATE issues SET title='fence wrote' WHERE id=$1`, fenceTarget)
				return err
			}}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: access})
		t.Cleanup(func() { _ = server.Close() })
		f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "member", Actor: "member"}))
			server.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(f.server.Close)

		parent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.private.ID, Title: "Fenced parent", Author: "worker", Owner: new("fenced-parent-owner")})
		require.NoError(t, err)
		for _, replyKind := range []string{"reply", "refute", "supersede", "confirm"} {
			t.Run(replyKind, func(t *testing.T) {
				finished.Store(0)
				child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared child " + replyKind, Author: "member"})
				require.NoError(t, err)
				_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
				require.NoError(t, err)
				targetAuthor := "reviewer"
				if replyKind == "confirm" {
					targetAuthor = "member" // A self-confirmation has no fan-out recipients.
				}
				target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Author: targetAuthor, Body: "Finding"})
				require.NoError(t, err)
				fenceTarget = child.ID

				status, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.public.ID, child.ShortID),
					"member", map[string]any{"actor": "member", "body": "Responded with the requested evidence and details.", "reply_to": target.UID, "kind": replyKind}, nil)
				require.Equal(t, http.StatusOK, status, string(body))
				require.Zero(t, finished.Load(), "a reply without parent fan-out must not consult the fenced parent project")
				comments, err := store.CommentsByIssue(t.Context(), child.ID)
				require.NoError(t, err)
				require.Len(t, comments, 2, "the typed reply must commit")
			})
		}
	})
}

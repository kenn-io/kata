package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// peerFencedHostAccess admits every request but its transaction fence
// rejects any decision that covers fencedProjectID: the host revoked that
// project after the request was admitted.
type peerFencedHostAccess struct{ fencedProjectID int64 }

func (a peerFencedHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	fenced := slices.Contains(request.Operation.ProjectIDs, a.fencedProjectID)
	return daemon.HostAccessDecision{
		Revalidate: func(context.Context) error { return nil },
		TransactionFence: func(context.Context, db.Transaction) error {
			if fenced {
				return daemon.ErrHostAccessDenied
			}
			return nil
		},
	}, nil
}

// A host admits a peer project for discovery with a decision whose
// transaction fence is the authority inside the write. A peer the fence
// rejects must not contribute participant identities.
func TestNotificationDiscoveryAppliesPeerHostTransactionFence(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		server := daemon.NewServer(daemon.ServerConfig{DB: store, HostAccess: peerFencedHostAccess{fencedProjectID: f.private.ID}})
		t.Cleanup(func() { _ = server.Close() })
		f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "member", Actor: "member"}))
			server.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(f.server.Close)

		t.Run("broadcast open child", func(t *testing.T) {
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared parent", Author: "member"})
			require.NoError(t, err)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Fenced child", Author: "worker", Owner: new("fenced-owner")})
			require.NoError(t, err)
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
			require.NoError(t, err)
			_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: parent.ID, Author: "public-reader", Body: "Watching"})
			require.NoError(t, err)

			status, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", f.public.ID, parent.ShortID),
				"member", map[string]any{"broadcast": true, "message": "Review the latest findings"}, nil)
			current, err := store.IssueByID(ctx, parent.ID)
			require.NoError(t, err)
			require.NotContains(t, string(current.Metadata), notification.MetadataKey("fenced-owner"), "fenced participant persisted")
			require.Equal(t, http.StatusOK, status, string(body))
			require.Contains(t, string(current.Metadata), notification.MetadataKey("public-reader"))
		})

		t.Run("confirm with fenced parent", func(t *testing.T) {
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Fenced parent", Author: "worker", Owner: new("fenced-parent-owner")})
			require.NoError(t, err)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared child", Author: "member"})
			require.NoError(t, err)
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
			require.NoError(t, err)
			target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Author: "reviewer", Body: "Finding"})
			require.NoError(t, err)

			status, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.public.ID, child.ShortID),
				"member", map[string]any{"actor": "member", "body": "Reproduced with the exact steps from the finding above.", "reply_to": target.UID, "kind": "confirm"}, nil)
			current, err := store.IssueByID(ctx, child.ID)
			require.NoError(t, err)
			require.NotContains(t, string(current.Metadata), notification.MetadataKey("fenced-parent-owner"), "fenced participant persisted")
			require.Equal(t, http.StatusOK, status, string(body))
			require.Contains(t, string(current.Metadata), notification.MetadataKey("reviewer"))
		})
	})
}

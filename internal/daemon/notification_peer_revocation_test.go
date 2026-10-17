package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// revokeBeforeNotificationWrite runs revoke after request admission, just
// before the write transaction that discovers notification recipients.
type revokeBeforeNotificationWrite struct {
	db.Storage
	revoke func() error
}

func (s *revokeBeforeNotificationWrite) runRevoke() error {
	if s.revoke == nil {
		return nil
	}
	revoke := s.revoke
	s.revoke = nil
	return revoke()
}

func (s *revokeBeforeNotificationWrite) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	if err := s.runRevoke(); err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}

func (s *revokeBeforeNotificationWrite) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	if err := s.runRevoke(); err != nil {
		return db.Comment{}, db.Event{}, err
	}
	return s.Storage.CreateComment(ctx, p)
}

// Access admitted with the request is only a snapshot. A peer project revoked
// before the write must not contribute participant identities to it. The
// response itself may be withheld, because the caller lost an admitted
// project during the request; the committed write is what must not leak.
func TestNotificationDiscoveryRechecksPeerProjectsInsideTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		wrapped := &revokeBeforeNotificationWrite{Storage: store}
		server := daemon.NewServer(daemon.ServerConfig{DB: wrapped, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		revokeMember := func() error {
			_, err := store.SetTeamMembership(context.Background(), f.team.UID, "member", false, "admin")
			return err
		}

		t.Run("broadcast open child", func(t *testing.T) {
			_, err := store.SetTeamMembership(ctx, f.team.UID, "member", true, "admin")
			require.NoError(t, err)
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared parent", Author: "member"})
			require.NoError(t, err)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Private child", Author: "worker", Owner: new("private-owner")})
			require.NoError(t, err)
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
			require.NoError(t, err)
			_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: parent.ID, Author: "public-reader", Body: "Watching"})
			require.NoError(t, err)

			wrapped.revoke = revokeMember
			_, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", f.public.ID, parent.ShortID),
				"member", map[string]any{"broadcast": true, "message": "Review the latest findings"}, nil)
			current, err := store.IssueByID(ctx, parent.ID)
			require.NoError(t, err)
			require.NotContains(t, string(current.Metadata), notification.MetadataKey("private-owner"), "revoked participant persisted")
			require.Contains(t, string(current.Metadata), notification.MetadataKey("public-reader"), "the broadcast committed: %s", body)
		})

		t.Run("confirm with private parent", func(t *testing.T) {
			_, err := store.SetTeamMembership(ctx, f.team.UID, "member", true, "admin")
			require.NoError(t, err)
			parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Private parent", Author: "worker", Owner: new("private-parent-owner")})
			require.NoError(t, err)
			child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared child", Author: "member"})
			require.NoError(t, err)
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
			require.NoError(t, err)
			target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Author: "reviewer", Body: "Finding"})
			require.NoError(t, err)

			wrapped.revoke = revokeMember
			_, _, body := f.request(t, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.public.ID, child.ShortID),
				"member", map[string]any{"actor": "member", "body": "Reproduced with the exact steps from the finding above.", "reply_to": target.UID, "kind": "confirm"}, nil)
			current, err := store.IssueByID(ctx, child.ID)
			require.NoError(t, err)
			require.NotContains(t, string(current.Metadata), notification.MetadataKey("private-parent-owner"), "revoked participant persisted")
			require.Contains(t, string(current.Metadata), notification.MetadataKey("reviewer"), "the reply committed: %s", body)
		})
	})
}

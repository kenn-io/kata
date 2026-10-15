package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/notification"
)

// Recipient discovery runs inside the notification write transaction. With a
// single pooled PostgreSQL connection, a pooled read of a cross-project peer
// would wait forever for the connection that transaction holds.
func TestNotificationDiscoveryAuthorizesCrossProjectPeersInsideTransaction(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		pg, ok := store.(*pgstore.Store)
		if !ok {
			t.Skip("connection-pool exhaustion applies to PostgreSQL")
		}
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		parent, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.public.ID, Title: "Shared parent", Author: "member", Owner: new("parent-owner")})
		require.NoError(t, err)
		child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Private child", Author: "worker", Owner: new("child-owner")})
		require.NoError(t, err)
		_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "member"})
		require.NoError(t, err)
		target, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: child.ID, Author: "reviewer", Body: "Finding"})
		require.NoError(t, err)

		pg.SetMaxOpenConns(1)
		post := func(path string, body any) (int, []byte) {
			t.Helper()
			data, err := json.Marshal(body)
			require.NoError(t, err)
			bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(bounded, http.MethodPost, f.server.URL+path, bytes.NewReader(data))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer member-test-token")
			resp, err := f.server.Client().Do(req)
			require.NoError(t, err, "request must not wait on the connection its own transaction holds")
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, raw
		}

		t.Run("broadcast open child", func(t *testing.T) {
			status, body := post(fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", f.public.ID, parent.ShortID),
				map[string]any{"broadcast": true, "message": "Review the latest findings"})
			require.Equal(t, http.StatusOK, status, string(body))
			require.Contains(t, string(body), "child-owner")
		})
		t.Run("confirm with cross-project parent", func(t *testing.T) {
			status, body := post(fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", f.private.ID, child.ShortID),
				map[string]any{"actor": "member", "body": "Reproduced with the exact steps from the finding above.", "reply_to": target.UID, "kind": "confirm"})
			require.Equal(t, http.StatusOK, status, string(body))
			current, err := store.IssueByID(ctx, child.ID)
			require.NoError(t, err)
			require.Contains(t, string(current.Metadata), notification.MetadataKey("parent-owner"))
		})
	})
}

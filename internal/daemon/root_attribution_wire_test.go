package daemon_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R5 exposes server-derived creation attribution while preserving raw source
// authors. A later editor does not replace the issue/comment creation proof.
func TestAttributionEntityWireProjection(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		_, err := store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: f.private.ID, Role: db.FederationRoleHub, HubProjectID: f.private.ID, HubProjectUID: f.private.UID, Enabled: true})
		require.NoError(t, err)
		_, key, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		signer := db.RootAttributionSigner{AuthorityUID: store.InstanceUID(), PrivateKey: key}
		ctx := db.WithRootAttribution(t.Context(), signer, "member")
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: f.private.ID, Title: "Attributed root task", Author: "assistant"})
		require.NoError(t, err)
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "comment-agent", Body: "Original comment", Teammate: "researcher"})
		require.NoError(t, err)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, RootAttributionSigner: &signer, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		httpServer := httptest.NewServer(server.Handler())
		t.Cleanup(httpServer.Close)
		f.server = httpServer
		root := fmt.Sprintf("/api/v1/projects/%d/issues/", f.private.ID)
		status, _, raw := f.request(t, http.MethodGet, root+issue.ShortID, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var result struct {
			Issue    map[string]any   `json:"issue"`
			Comments []map[string]any `json:"comments"`
		}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, "assistant", result.Issue["author"])
		require.Equal(t, "member", result.Issue["accountable_actor"])
		require.Equal(t, "assistant", result.Issue["source_actor"])
		require.Equal(t, store.InstanceUID(), result.Issue["authority_uid"])
		require.Equal(t, "verified", result.Issue["verification"])
		require.Len(t, result.Comments, 1)
		require.Equal(t, comment.UID, result.Comments[0]["uid"])
		require.Equal(t, "member", result.Comments[0]["accountable_actor"])
		require.Equal(t, "comment-agent", result.Comments[0]["source_actor"])
		require.Equal(t, "researcher", result.Comments[0]["teammate"])
		require.Equal(t, "verified", result.Comments[0]["verification"])
		status, _, raw = f.request(t, http.MethodGet, root+f.issue.ShortID, "member", nil, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		result.Issue = nil
		result.Comments = nil
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, "legacy", result.Issue["verification"])
		require.Empty(t, result.Issue["accountable_actor"], "unproven legacy rows cannot acquire an account")
	})
}

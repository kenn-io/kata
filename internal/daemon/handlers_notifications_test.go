package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestNotifyCommentAndBroadcastWritePath(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	c, _, e := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: id, Author: "reader", Teammate: "review", Body: "Finding"})
	require.NoError(t, e)
	_, _, _, e = h.DB().UpdateOwner(t.Context(), id, new("owner"), "worker")
	require.NoError(t, e)
	path := issueURL(pid, id, "notifications")
	resp, body := postJSON(t, ts, path, map[string]any{"actor": "worker", "to": "reader/review", "re": c.UID, "message": "inspect finding"})
	require.Equal(t, 200, resp.StatusCode, string(body))
	require.Contains(t, string(body), c.UID)
	// Explicit broadcast overwrites a prior no-re human request.
	resp, body = postJSON(t, ts, path, map[string]any{"actor": "worker", "broadcast": true, "teammates": true, "message": "all inspect"})
	require.Equal(t, 200, resp.StatusCode, string(body))
	var result struct {
		Recipients []string `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, []string{"owner", "reader", "reader/review"}, result.Recipients)
	before, e := h.DB().MaxEventID(t.Context())
	require.NoError(t, e)
	resp, body = postJSON(t, ts, path, map[string]any{"actor": "worker", "broadcast": true, "message": "second"})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, string(body))
	for _, field := range []string{"broadcast_rate_limited", "retry_after_seconds", "window", "last_broadcast_at", "last_broadcast_by", "last_message_prefix", "--to", "--re"} {
		require.Contains(t, string(body), field)
	}
	after, e := h.DB().MaxEventID(t.Context())
	require.NoError(t, e)
	require.Equal(t, before, after)
}

func TestNotifyScopedCommentResolution(t *testing.T) {
	env, p, source, hidden, visible := scopedReplyFixture(t, true)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, source.ShortID)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	resp, body := envDoRaw(t, env, http.MethodPost, path, map[string]any{"to": "reader", "re": hidden.UID, "message": "inspect"}, headers)
	require.Equal(t, 404, resp.StatusCode, string(body))
	resp, body = envDoRaw(t, env, http.MethodPost, path, map[string]any{"to": "reader", "re": visible.UID, "message": "inspect"}, headers)
	require.Equal(t, 200, resp.StatusCode, string(body))
}

// A subtree can change after route authorization; the writing transaction
// must reject a source that has left the authorized tree.
type notificationDetachStore struct {
	db.Storage
	detach func() error
}

func (s *notificationDetachStore) PatchIssueMetadata(ctx context.Context, in db.PatchIssueMetadataIn) (db.PatchIssueMetadataOut, error) {
	if err := s.detach(); err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}
func TestNotifyRejectsDetachedScopedSource(t *testing.T) {
	env, p, source, _, _ := scopedReplyFixture(t, true)
	parent, err := env.DB.ParentOf(t.Context(), source.ID)
	require.NoError(t, err)
	wrapped := &notificationDetachStore{Storage: env.DB, detach: func() error { return env.DB.DeleteLinkByID(t.Context(), parent.ID) }}
	cfg := daemon.ServerConfig{DB: wrapped, StartedAt: time.Now().UTC()}
	cfg.Auth.RequireTokenIdentity = true
	cfg.Auth.Token = "bootstrap-token"
	d := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = d.Close() })
	server := httptest.NewServer(d.Handler())
	t.Cleanup(server.Close)
	raw := []byte(`{"to":"reader","message":"Inspect finding"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/notifications", server.URL, p.ID, source.ShortID), bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer worker-token")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 404, resp.StatusCode, string(body))
	current, err := env.DB.IssueByID(t.Context(), source.ID)
	require.NoError(t, err)
	require.NotContains(t, string(current.Metadata), "notify.")
}

package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
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

type notificationMoveBeforeWriteStore struct {
	db.Storage
	toProjectID int64
	moved       bool
}

func withNotificationMoveBeforeWriteStore(target **notificationMoveBeforeWriteStore) serverOption {
	return func(cfg *daemon.ServerConfig) {
		wrapped := &notificationMoveBeforeWriteStore{Storage: cfg.DB}
		*target = wrapped
		cfg.DB = wrapped
	}
}

func (s *notificationMoveBeforeWriteStore) moveIssue(ctx context.Context, issueID int64) error {
	if s.moved || s.toProjectID == 0 {
		return nil
	}
	issue, err := s.IssueByID(ctx, issueID)
	if err != nil {
		return err
	}
	_, err = s.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: issue.ProjectID, ToProjectID: s.toProjectID,
		IfMatchRev: issue.Revision, Actor: "coordinator",
	})
	if err == nil {
		s.moved = true
	}
	return err
}

func (s *notificationMoveBeforeWriteStore) PatchIssueMetadata(
	ctx context.Context, in db.PatchIssueMetadataIn,
) (db.PatchIssueMetadataOut, error) {
	if err := s.moveIssue(ctx, in.IssueID); err != nil {
		return db.PatchIssueMetadataOut{}, err
	}
	return s.Storage.PatchIssueMetadata(ctx, in)
}

func (s *notificationMoveBeforeWriteStore) CreateComment(
	ctx context.Context, in db.CreateCommentParams,
) (db.Comment, db.Event, error) {
	if err := s.moveIssue(ctx, in.IssueID); err != nil {
		return db.Comment{}, db.Event{}, err
	}
	return s.Storage.CreateComment(ctx, in)
}

func TestNotifyPublishesCommittedProjectAfterConcurrentMove(t *testing.T) {
	checkNotifyPublishesCommittedProjectAfterConcurrentMove(t, "Inspect the finding", 0)
}

func FuzzNotifyPublishesCommittedProjectAfterConcurrentMove(f *testing.F) {
	f.Add("Inspect the finding", uint8(0))
	f.Fuzz(func(t *testing.T, message string, projectOffset uint8) {
		if len(message) > 256 {
			return
		}
		if strings.TrimSpace(message) == "" {
			message = "Inspect the finding"
		}
		checkNotifyPublishesCommittedProjectAfterConcurrentMove(t, message, projectOffset)
	})
}

func checkNotifyPublishesCommittedProjectAfterConcurrentMove(t *testing.T, message string, projectOffset uint8) {
	t.Helper()
	broadcaster := daemon.NewEventBroadcaster()
	var wrapped *notificationMoveBeforeWriteStore
	h, projectAID := bootstrapProject(t, withBroadcaster(broadcaster), withNotificationMoveBeforeWriteStore(&wrapped))
	source, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projectAID, Title: "Movable issue", Author: "worker"})
	require.NoError(t, err)
	for i := uint8(0); i < projectOffset%4; i++ {
		_, err := h.DB().CreateProject(t.Context(), fmt.Sprintf("spoke-project-%d", i))
		require.NoError(t, err)
	}
	projectB, err := h.DB().CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	wrapped.toProjectID = projectB.ID
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectB.ID})
	defer sub.Unsub()

	resp, body := postJSON(t, h.ts.(*httptest.Server), issueURLRef(projectAID, source.ShortID, "notifications"),
		map[string]any{"actor": "worker", "to": "reader", "message": message})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "notify: %s", body)
	require.True(t, wrapped.moved)
	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, projectB.ID, msg.ProjectID)
		require.Equal(t, projectB.ID, msg.Event.ProjectID)
		require.Equal(t, "issue.metadata_updated", msg.Event.Type)
	case <-time.After(time.Second):
		t.Fatal("project B did not receive the committed metadata event")
	}
}

func TestCommentPublishesRetainedEventsByCommittedProjectAfterMove(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	var wrapped *notificationMoveBeforeWriteStore
	h, projectAID := bootstrapProject(t, withBroadcaster(broadcaster), withNotificationMoveBeforeWriteStore(&wrapped))
	source, _, err := h.DB().CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projectAID, Title: "Movable issue", Author: "worker"})
	require.NoError(t, err)
	target, _, err := h.DB().CreateComment(t.Context(), db.CreateCommentParams{IssueID: source.ID, Author: "reviewer", Body: "Finding"})
	require.NoError(t, err)
	value, err := json.Marshal(notification.Value{From: "worker", Message: "Inspect the finding", Re: target.UID})
	require.NoError(t, err)
	_, err = h.DB().PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: source.ID, Actor: "worker",
		Patch: map[string]jsontext.Value{notification.MetadataKey("reader"): jsontext.Value(value)},
	})
	require.NoError(t, err)
	projectB, err := h.DB().CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	wrapped.toProjectID = projectB.ID
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectB.ID})
	defer sub.Unsub()

	resp, body := postJSON(t, h.ts.(*httptest.Server), issueURLRef(projectAID, source.ShortID, "comments"),
		map[string]any{"actor": "reader", "body": "Answer", "reply_to": target.UID, "kind": "reply"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "reply: %s", body)
	require.True(t, wrapped.moved)
	types := make(map[string]bool)
	for range 2 {
		select {
		case msg := <-sub.Ch:
			require.NotNil(t, msg.Event)
			require.Equal(t, projectB.ID, msg.ProjectID)
			require.Equal(t, projectB.ID, msg.Event.ProjectID)
			types[msg.Event.Type] = true
		case <-time.After(time.Second):
			t.Fatal("project B did not receive every committed comment event")
		}
	}
	require.True(t, types["issue.commented"], "the committed comment event must reach project B")
	require.True(t, types["issue.metadata_updated"], "the retained auto-clear event must reach project B")
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

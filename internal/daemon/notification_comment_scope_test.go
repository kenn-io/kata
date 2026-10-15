package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type notificationDetachCommentStore struct {
	db.Storage
	detach        func() error
	fired         bool
	afterDetachID int64
}

func (s *notificationDetachCommentStore) CreateComment(ctx context.Context, p db.CreateCommentParams) (db.Comment, db.Event, error) {
	s.fired = true
	if err := s.detach(); err != nil {
		return db.Comment{}, db.Event{}, err
	}
	var err error
	s.afterDetachID, err = s.MaxEventID(ctx)
	if err != nil {
		return db.Comment{}, db.Event{}, err
	}
	return s.Storage.CreateComment(ctx, p)
}

// A target or source leaving the authorized subtree between resolution and
// the write transaction is unavailable, and must not commit a partial reply.
func checkNotificationCommentLateDetach(t *testing.T, detachSource bool, kind string, depth uint8, suffix string) {
	t.Helper()
	env, project, source, target, _ := scopedReplyFixture(t, false)
	detachedID := target.IssueID
	if detachSource {
		detachedID = source.ID
	}
	link, err := env.DB.ParentOf(t.Context(), detachedID)
	require.NoError(t, err)
	// Draw the full depth byte; cap only the materialized fixture's size.
	parentID := link.ToIssueID
	for range min(int(depth), 8) {
		ancestor, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: project.ID, Title: "Scope ancestor", Author: "coordinator",
		})
		require.NoError(t, err)
		_, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: ancestor.ID, ToIssueID: parentID, Type: "parent", Author: "coordinator",
		})
		require.NoError(t, err)
		parentID = ancestor.ID
	}
	if depth != 0 {
		require.NoError(t, env.DB.DeleteLinkByID(t.Context(), link.ID))
		link, err = env.DB.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: detachedID, ToIssueID: parentID, Type: "parent", Author: "coordinator",
		})
		require.NoError(t, err)
	}
	before, err := env.DB.CommentsByIssue(t.Context(), source.ID)
	require.NoError(t, err)
	wrapped := &notificationDetachCommentStore{Storage: env.DB, detach: func() error {
		return env.DB.DeleteLinkByID(t.Context(), link.ID)
	}}
	cfg := daemon.ServerConfig{DB: wrapped, StartedAt: time.Now()}
	cfg.Auth.RequireTokenIdentity = true
	cfg.Auth.Token = "bootstrap-token"
	d := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = d.Close() })
	// Materialize valid, bounded UTF-8 while keeping arbitrary text in the
	// generator. The prefix meets every canonical kind's evidence minimum.
	runes := []rune(suffix)
	evidence := "An independently reproduced response with enough evidence for verification. " + string(runes[:min(len(runes), 128)])
	body, err := json.Marshal(map[string]any{
		"reply_to": target.UID, "kind": kind, "force": true,
		"body": evidence,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%d/issues/%s/comments", project.ID, source.ShortID), strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer worker-token")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, req)
	require.True(t, wrapped.fired, "detach after handler resolution, before the write transaction")
	require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &refusal))
	require.Contains(t, []string{"issue_not_found", "comment_not_found"}, refusal.Error.Code)
	after, err := env.DB.CommentsByIssue(t.Context(), source.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	current, err := env.DB.IssueByID(t.Context(), source.ID)
	require.NoError(t, err)
	require.NotContains(t, string(current.Metadata), "notify.")
	lastEventID, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, wrapped.afterDetachID, lastEventID, "the refused reply emits no events")
}

func TestNotificationCommentDetachedTargetReturnsNotFound(t *testing.T) {
	checkNotificationCommentLateDetach(t, false, "reply", 0, "")
}

func TestNotificationCommentDetachedSourceReturnsNotFound(t *testing.T) {
	checkNotificationCommentLateDetach(t, true, "reply", 3, "Nested source")
}

func FuzzNotificationCommentLateDetach(f *testing.F) {
	for kind := range uint8(4) {
		f.Add(false, kind, uint8(0), "")
		f.Add(true, kind, uint8(3), "Nested source")
	}
	f.Add(false, uint8(1), uint8(255), "Multiline\nresponse ✓")
	f.Fuzz(func(t *testing.T, source bool, kindIndex uint8, depth uint8, suffix string) {
		kinds := [...]string{"reply", "confirm", "refute", "supersede"}
		checkNotificationCommentLateDetach(t, source, kinds[int(kindIndex)%len(kinds)], depth, suffix)
	})
}

package daemon_test

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

type notificationProjectAccess struct{ denied int64 }

func (a notificationProjectAccess) Authorize(_ context.Context, r daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	if r.Operation.AllProjects || slices.Contains(r.Operation.ProjectIDs, a.denied) {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	return daemon.HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}
func TestNotifyDiscoveryRespectsHostProjectScope(t *testing.T) {
	for _, mode := range []string{"broadcast", "confirm"} {
		t.Run(mode, func(t *testing.T) {
			h, _, pid, sourceID := bootstrapProjectWithIssue(t)
			store := h.DB()
			secret, err := store.CreateProject(t.Context(), "hidden-project")
			require.NoError(t, err)
			hidden, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: secret.ID, Title: "Hidden", Author: "hidden-author", Owner: new("hidden-owner")})
			require.NoError(t, err)
			target, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: sourceID, Author: "reader", Body: "Finding"})
			require.NoError(t, err)
			from, to := hidden.ID, sourceID
			if mode == "confirm" {
				from, to = sourceID, hidden.ID
			}
			_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: from, ToIssueID: to, Type: "parent", Author: "setup"})
			require.NoError(t, err)
			d := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now(), HostAccess: notificationProjectAccess{denied: secret.ID}})
			t.Cleanup(func() { _ = d.Close() })
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r = r.WithContext(daemon.WithPrincipal(r.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "subject", Actor: "worker"}))
				d.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			path := issueURL(pid, sourceID, "notifications")
			body := map[string]any{"broadcast": true, "message": "Inspect finding"}
			if mode == "confirm" {
				path = issueURL(pid, sourceID, "comments")
				body = map[string]any{"reply_to": target.UID, "kind": "confirm", "body": "Confirmed through a full independent reproduction of the finding."}
			}
			resp, raw := postJSON(t, ts, path, body)
			require.Equal(t, 200, resp.StatusCode, string(raw))
			require.NotContains(t, string(raw), "notify.aGlkZGVuLW93bmVy", "host-denied owners must not be discovered or returned")
		})
	}
}

type notificationDetachProjectionStore struct {
	db.Storage
	detach func() error
	fired  bool
}

func (s *notificationDetachProjectionStore) IssueScopedMembers(ctx context.Context, scope db.APITokenScope) ([]db.Issue, error) {
	rows, err := s.Storage.IssueScopedMembers(ctx, scope)
	if err == nil && !s.fired {
		s.fired = true
		if err = s.detach(); err != nil {
			return nil, err
		}
	}
	return rows, err
}
func TestNotifyBufferedProjectionRejectsLateDetach(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, false)
	link, err := env.DB.ParentOf(t.Context(), target.IssueID)
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "lead", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(fmt.Sprintf(`{"from":"lead","message":"hidden target context","re":%q}`, target.UID))}})
	require.NoError(t, err)
	wrapped := &notificationDetachProjectionStore{Storage: env.DB, detach: func() error { return env.DB.DeleteLinkByID(t.Context(), link.ID) }}
	cfg := daemon.ServerConfig{DB: wrapped, StartedAt: time.Now()}
	cfg.Auth.RequireTokenIdentity = true
	cfg.Auth.Token = "bootstrap-token"
	d := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = d.Close() })
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", p.ID, source.ShortID), nil)
	req.Header.Set("Authorization", "Bearer worker-token")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, req)
	require.True(t, wrapped.fired)
	t.Logf("status=%d body=%s", response.Code, response.Body.String())
	require.False(t, strings.Contains(response.Body.String(), target.UID), "notification target left the scope before buffered response release")
}

func TestNotifyRateErrorRedactsHiddenPointer(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, true)
	_, err := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(fmt.Sprintf(`{"from":"worker","message":"hidden target context","re":%q,"broadcast":true}`, target.UID))}})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	resp, body := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", p.ID, source.ShortID), nil, headers)
	require.Equal(t, 200, resp.StatusCode, string(body))
	require.NotContains(t, string(body), "hidden target context")
	resp, body = envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, source.ShortID), map[string]any{"broadcast": true, "message": "New finding"}, headers)
	require.Equal(t, 429, resp.StatusCode, string(body))
	require.NotContains(t, string(body), "hidden target context", "rate errors must respect the notification pointer projection")
}

type notificationDetachStreamProjectionStore struct {
	db.Storage
	detach       func() error
	armed, fired bool
}

func (s *notificationDetachStreamProjectionStore) CommentIssueIDsByUIDs(ctx context.Context, uids []string) (map[string]int64, error) {
	rows, err := s.Storage.CommentIssueIDsByUIDs(ctx, uids)
	s.armed = true
	return rows, err
}
func (s *notificationDetachStreamProjectionStore) IssueScopedMembers(ctx context.Context, scope db.APITokenScope) ([]db.Issue, error) {
	rows, err := s.Storage.IssueScopedMembers(ctx, scope)
	if err == nil && s.armed && !s.fired {
		s.fired = true
		if err = s.detach(); err != nil {
			return nil, err
		}
	}
	return rows, err
}
func TestNotifySSERejectsLatePointerDetach(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, false)
	link, err := env.DB.ParentOf(t.Context(), target.IssueID)
	require.NoError(t, err)
	out, err := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "lead", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(fmt.Sprintf(`{"from":"lead","message":"hidden target context","re":%q}`, target.UID))}})
	require.NoError(t, err)
	wrapped := &notificationDetachStreamProjectionStore{Storage: env.DB, detach: func() error { return env.DB.DeleteLinkByID(t.Context(), link.ID) }}
	cfg := daemon.ServerConfig{DB: wrapped, StartedAt: time.Now()}
	cfg.Auth.RequireTokenIdentity = true
	cfg.Auth.Token = "bootstrap-token"
	d := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = d.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/events/stream?project_id=%d&after_id=%d", p.ID, out.Event.ID-1), nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer worker-token")
	req.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, req)
	require.True(t, wrapped.fired)
	t.Logf("status=%d body=%s", response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), target.UID, "target left the authorized scope before the event was emitted")
}

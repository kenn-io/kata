package daemon_test

import (
	"context"
	"encoding/json/jsontext"
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

type notificationDetachRateStore struct {
	db.Storage
	detach func() error
	fired  bool
}

func (s *notificationDetachRateStore) IssueInScope(ctx context.Context, scope db.APITokenScope, ids ...int64) (bool, error) {
	if !s.fired && len(ids) > 1 {
		s.fired = true
		if err := s.detach(); err != nil {
			return false, err
		}
	}
	return s.Storage.IssueInScope(ctx, scope, ids...)
}

// Rate diagnostics are buffered responses too: a target that leaves the
// subtree after history selection must not disclose its notification context.
func TestNotifyRateErrorRejectsLateDetach(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, false)
	link, err := env.DB.ParentOf(t.Context(), target.IssueID)
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(fmt.Sprintf(`{"from":"worker","message":"target context","re":%q,"broadcast":true}`, target.UID))}})
	require.NoError(t, err)
	wrapped := &notificationDetachRateStore{Storage: env.DB, detach: func() error { return env.DB.DeleteLinkByID(t.Context(), link.ID) }}
	cfg := daemon.ServerConfig{DB: wrapped, StartedAt: time.Now()}
	cfg.Auth.RequireTokenIdentity = true
	cfg.Auth.Token = "bootstrap-token"
	d := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = d.Close() })
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications", p.ID, source.ShortID), strings.NewReader(`{"broadcast":true,"message":"New finding"}`))
	req.Header.Set("Authorization", "Bearer worker-token")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	d.Handler().ServeHTTP(response, req)
	require.NotContains(t, response.Body.String(), "target context", "final rate response must revalidate its notification target")
	require.True(t, wrapped.fired, "history target must participate in final response scope checks")
}

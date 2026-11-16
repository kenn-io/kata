package daemon_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func TestScopedNotificationPointersHiddenAcrossReads(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, true)
	_, e := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "coordinator", Patch: map[string]jsontext.Value{"notify.d29ya2Vy": jsontext.Value(fmt.Sprintf(`{"from":"lead","message":"private","re":%q,"kind":"reply"}`, target.UID))}})
	require.NoError(t, e)
	base := fmt.Sprintf("/api/v1/projects/%d/issues/%s", p.ID, source.ShortID)
	for _, path := range []string{base, base + "/metadata", fmt.Sprintf("/api/v1/projects/%d/issues", p.ID), fmt.Sprintf("/api/v1/projects/%d/events", p.ID), "/api/v1/ui/snapshot?view=all-open&selected_issue_uid=" + source.UID} {
		t.Run(path, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer worker-token"})
			require.Equal(t, 200, resp.StatusCode, string(body))
			require.NotContains(t, string(body), target.UID)
			require.NotContains(t, string(body), "private")
		})
	}
}

// Projection rewrites a restricted reader's response. The same state must
// still produce the same bytes, so diffs and caches see no phantom changes.
func TestScopedNotificationProjectionIsDeterministic(t *testing.T) {
	env, p, source, target, _ := scopedReplyFixture(t, false)
	patch := map[string]jsontext.Value{"notify.d29ya2Vy": jsontext.Value(fmt.Sprintf(`{"from":"lead","message":"visible","re":%q,"kind":"reply"}`, target.UID))}
	for i := range 16 {
		patch[fmt.Sprintf("review.field-%02d", i)] = jsontext.Value(fmt.Sprintf("%d", i))
	}
	_, e := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "coordinator", Patch: patch})
	require.NoError(t, e)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", p.ID, source.ShortID)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	_, first := envDoRaw(t, env, http.MethodGet, path, nil, headers)
	require.Contains(t, string(first), target.UID)
	for range 8 {
		resp, body := envDoRaw(t, env, http.MethodGet, path, nil, headers)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.Equal(t, string(first), string(body))
	}
}

// An issue-subtree credential can clear a held notification with if_from; the
// route sits behind the same allowlist as sending one.
func TestScopedTokenClearsNotificationIfFrom(t *testing.T) {
	env, p, source, _, _ := scopedReplyFixture(t, false)
	key := notification.MetadataKey("reader")
	_, e := env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: source.ID, Actor: "coordinator", Patch: map[string]jsontext.Value{
		key: jsontext.Value(`{"from":"worker","message":"look"}`),
	}})
	require.NoError(t, e)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/notifications/clear", p.ID, source.ShortID)
	body := map[string]any{"to": "reader", "if_from": "worker"}
	resp, raw := envDoRaw(t, env, http.MethodPost, path, body, map[string]string{"Authorization": "Bearer worker-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.Equal(t, notification.Value{}, readNotifySlot(t, env.DB, source.ID, key))
}

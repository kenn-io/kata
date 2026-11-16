package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func TestNotifyNoReplaceRefusesHeldRequest(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	path := issueURL(pid, id, "notifications")
	key := notification.MetadataKey("reader")

	resp, body := postJSON(t, ts, path, map[string]any{"actor": "worker", "to": "reader", "message": "first", "replace": false})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	before, err := h.DB().MaxEventID(t.Context())
	require.NoError(t, err)
	resp, body = postJSON(t, ts, path, map[string]any{"actor": "other", "to": "reader", "message": "second", "replace": false})
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	require.Contains(t, string(body), "notification_held")
	require.Contains(t, string(body), `"from":"worker"`)
	require.Contains(t, string(body), `"message":"first"`)
	after, err := h.DB().MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, notification.Value{From: "worker", Message: "first"}, readNotifySlot(t, h.DB(), id, key))

	// Default behaviour still replaces.
	resp, body = postJSON(t, ts, path, map[string]any{"actor": "other", "to": "reader", "message": "second"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Equal(t, notification.Value{From: "other", Message: "second"}, readNotifySlot(t, h.DB(), id, key))
}

func TestNotifyNoReplaceRejectsBroadcast(t *testing.T) {
	_, ts, pid, id := bootstrapProjectWithIssue(t)
	resp, body := postJSON(t, ts, issueURL(pid, id, "notifications"), map[string]any{"actor": "worker", "broadcast": true, "message": "all", "replace": false})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
}

func TestNotifyClearIfFromClearsOnlyMatchingSender(t *testing.T) {
	h, ts, pid, id := bootstrapProjectWithIssue(t)
	path := issueURL(pid, id, "notifications")
	key := notification.MetadataKey("reader")
	seed := func(from string) {
		_, err := h.DB().PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: id, Actor: from, Patch: map[string]jsontext.Value{
			key: jsontext.Value(`{"from":"` + from + `","message":"please look"}`),
		}})
		require.NoError(t, err)
	}

	seed("alice")
	resp, body := postJSON(t, ts, path, map[string]any{"actor": "bob", "to": "reader", "clear": true, "if_from": "bob"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out struct {
		Changed bool `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	require.False(t, out.Changed)
	require.Equal(t, notification.Value{From: "alice", Message: "please look"}, readNotifySlot(t, h.DB(), id, key))

	resp, body = postJSON(t, ts, path, map[string]any{"actor": "bob", "to": "reader", "clear": true, "if_from": "alice"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &out))
	require.True(t, out.Changed)
	issue, err := h.DB().IssueByID(t.Context(), id)
	require.NoError(t, err)
	require.NotContains(t, string(issue.Metadata), key)
}

func TestNotifyClearValidation(t *testing.T) {
	_, ts, pid, id := bootstrapProjectWithIssue(t)
	path := issueURL(pid, id, "notifications")
	for name, body := range map[string]map[string]any{
		"if_from without clear":    {"actor": "a", "to": "reader", "message": "m", "if_from": "a"},
		"clear with message":       {"actor": "a", "to": "reader", "message": "m", "clear": true},
		"clear with broadcast":     {"actor": "a", "broadcast": true, "clear": true},
		"clear without recipient":  {"actor": "a", "clear": true},
		"clear with no-replace":    {"actor": "a", "to": "reader", "clear": true, "replace": false},
		"message missing no clear": {"actor": "a", "to": "reader"},
	} {
		resp, raw := postJSON(t, ts, path, body)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, name+": "+string(raw))
	}
}

func readNotifySlot(t *testing.T, store db.Storage, issueID int64, key string) notification.Value {
	t.Helper()
	issue, err := store.IssueByID(t.Context(), issueID)
	require.NoError(t, err)
	var slots map[string]notification.Value
	require.NoError(t, json.Unmarshal([]byte(issue.Metadata), &slots))
	return slots[key]
}

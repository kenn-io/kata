package tui

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientIssueHistoryRequiresAcknowledgementOnEveryPage(t *testing.T) {
	const uid = "01JZ0000000000000000000001"
	for _, tc := range []struct {
		name, ref string
		headers   []string
		wantCalls int
	}{
		{"old daemon", "abc4", []string{""}, 1},
		{"missing second page", "abc4", []string{uid, ""}, 2},
		{"changed subject", "abc4", []string{uid, "01JZ0000000000000000000002"}, 2},
		{"wrong UID", uid, []string{"01JZ0000000000000000000002"}, 1},
		{"invalid UID", "abc4", []string{"invalid"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls <= len(tc.headers) {
					w.Header().Set("X-Kata-Events-Issue-UID", tc.headers[calls-1])
				}
				if got := r.URL.Query().Get("issue"); got != tc.ref {
					t.Errorf("issue query=%q, want %q", got, tc.ref)
				}
				events := []map[string]any{}
				if calls == 1 {
					events = append(events, map[string]any{"event_id": 1, "issue_short_id": "abc4", "issue_uid": uid, "type": "issue.created", "actor": "reader"})
				}
				respondJSON(t, w, map[string]any{"events": events, "next_after_id": 1, "reset_required": false})
			})
			got, err := c.ListEvents(context.Background(), 7, tc.ref)
			require.ErrorContains(t, err, "upgrade")
			require.Nil(t, got)
			require.Equal(t, tc.wantCalls, calls)
		})
	}
}

func TestClientIssueHistoryQualifiedAndLowercaseRefs(t *testing.T) {
	const uid = "01JZ0000000000000000000001"
	for _, ref := range []string{"example-project#abc4", "01jz0000000000000000000001"} {
		t.Run(ref, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, ref, r.URL.Query().Get("issue"))
				w.Header().Set("X-Kata-Events-Issue-UID", uid)
				respondJSON(t, w, map[string]any{"events": []map[string]any{{"event_id": 1, "issue_uid": uid, "issue_short_id": "abc4", "type": "issue.created", "actor": "reader"}}, "next_after_id": 0, "reset_required": false})
			})
			got, err := c.ListEvents(context.Background(), 7, ref)
			require.NoError(t, err)
			require.Len(t, got, 1)
		})
	}
}

func TestClientIssueHistoryRejectsReset(t *testing.T) {
	const uid = "01JZ0000000000000000000001"
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("X-Kata-Events-Issue-UID", uid)
		respondJSON(t, w, map[string]any{"events": []map[string]any{}, "next_after_id": 9, "reset_required": true, "reset_after_id": 9})
	})
	got, err := c.ListEvents(context.Background(), 7, "abc4")
	require.ErrorContains(t, err, "history changed")
	require.Nil(t, got)
	require.Equal(t, 1, calls)
}

package daemon

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestScopedUpdatedEventPreservesVisibilityFields(t *testing.T) {
	for _, payload := range []string{
		`{"status":"open","closed_at":null,"source":"github:example-source","external_id":"issue-id:123"}`,
		`{"status":"closed","closed_at":"2026-01-01T00:00:00Z","source":"github:example-source","external_id":"issue-id:123"}`,
		`{"deleted_at":"2026-01-01T00:00:00Z","source":"github:example-source","external_id":"issue-id:123"}`,
	} {
		var want map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &want))
		delete(want, "source")
		delete(want, "external_id")
		projected, ok := scopedEventPayload(db.Event{Type: "issue.updated", Payload: payload})
		require.True(t, ok)
		var got map[string]any
		require.NoError(t, json.Unmarshal([]byte(projected), &got))
		require.Equal(t, want, got, "authorized visibility changes must survive without provider identifiers")
	}
}

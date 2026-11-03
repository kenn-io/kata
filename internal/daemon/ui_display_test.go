package daemon

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestUISnapshotDisplayContribution(t *testing.T) {
	event := db.Event{ID: 7, UID: "event", Type: "issue.updated", Actor: "actor", Payload: `{"attributed_teammate":"writer"}`, ContentHash: "unchanged"}
	response, err := snapshotResponse(t.Context(), db.UISnapshotData{History: []db.Event{event}}, normalizedUISnapshotIntent{SelectedIssueUID: "issue"}, uiPolicy{}, "validator")
	require.NoError(t, err)
	encoded, err := json.Marshal(response.Body.Selected.History)
	require.NoError(t, err)
	var history []map[string]any
	require.NoError(t, json.Unmarshal(encoded, &history))
	require.Equal(t, "writer", history[0]["attributed_teammate"])
	require.Equal(t, float64(7), history[0]["id"])
	require.Equal(t, event.Payload, history[0]["payload"])
	require.Equal(t, event.ContentHash, history[0]["content_hash"])
}

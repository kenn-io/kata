package mcpserver

import (
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: scoped kata.events keeps a close's duplicate target when that
// target is inside the startup scope and never discloses one outside it.
// Close evidence stores target UIDs, so scope follows each target's project.
func TestScopedEventsKeepInScopeCloseEvidenceTargets(t *testing.T) {
	store, _, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
	original, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "in-scope original", Author: "worker"})
	require.NoError(t, err)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.close", Arguments: map[string]any{
		"ref": source.Name + "#" + issue.ShortID, "reason": "duplicate", "message": "Same report as the in-scope original issue.",
		"evidence": []any{map[string]any{"type": "duplicate-of", "issue_ref": source.Name + "#" + original.ShortID}},
	}})
	require.NoError(t, err)
	require.False(t, result.IsError, string(mustJSON(t, result)))
	events := func(after int64) (EventsOutput, string) {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.events", Arguments: map[string]any{"project": source.Name, "after": after, "limit": 100}})
		require.NoError(t, err)
		require.False(t, result.IsError, string(mustJSON(t, result)))
		var output EventsOutput
		require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
		return output, string(mustJSON(t, result))
	}
	output, _ := events(0)
	var kept any
	for _, event := range output.Events {
		if event.Type == "issue.closed" && event.IssueUID == issue.UID {
			kept = event.Payload.(map[string]any)["evidence"].([]any)[0].(map[string]any)["issue_ref"]
		}
	}
	require.Equal(t, original.UID, kept, "in-scope duplicate target must survive redaction")

	hidden, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: excluded.ID, Title: "hidden original", Author: "worker"})
	require.NoError(t, err)
	other, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second duplicate", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssueWithEvents(t.Context(), other.ID, "duplicate", "worker", "Same report as an issue outside the scope.", []db.Evidence{{Type: "duplicate-of", IssueRef: hidden.UID}})
	require.NoError(t, err)
	_, raw := events(output.NextAfterID)
	require.NotContains(t, raw, hidden.UID, "an out-of-scope duplicate target must not be disclosed")
}

package mcpserver

import (
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: kata.move moves the subtree by default and reports every moved
// issue; single moves only the root and dry_run previews without moving.
func TestMoveToolSubtreeOptions(t *testing.T) {
	for _, mode := range []string{"subtree", "single", "dry_run"} {
		t.Run(mode, func(t *testing.T) {
			store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
			child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: issue.ID}}})
			require.NoError(t, err)
			args := map[string]any{"ref": source.Name + "#" + issue.ShortID, "to_project": target.Name}
			if mode != "subtree" {
				args[mode] = true
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: args})
			require.NoError(t, err)
			require.False(t, result.IsError, string(mustJSON(t, result)))
			var output MoveOutput
			require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
			uids := make([]string, 0, len(output.Issues))
			for _, moved := range output.Issues {
				uids = append(uids, moved.UID)
			}
			expectedProject := map[string]int64{"subtree": target.ID, "single": source.ID, "dry_run": source.ID}[mode]
			if mode == "single" {
				require.Equal(t, []string{issue.UID}, uids)
			} else {
				require.Equal(t, []string{issue.UID, child.UID}, uids)
			}
			require.Equal(t, mode != "dry_run", output.Changed)
			got, err := store.IssueByID(t.Context(), child.ID)
			require.NoError(t, err)
			require.Equal(t, expectedProject, got.ProjectID)
		})
	}
}

// Contract: kata.move checks the daemon's API version on every move. Before
// API 0.35.0 it refuses a default move rather than silently moving only the
// root; after the same daemon upgrades, single still moves only the root.
func TestMoveToolFollowsDaemonUpgrade(t *testing.T) {
	store, wrapped, session, source, target, _, issue := mcpFixedScopeFixture(t)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: issue.ID}}})
	require.NoError(t, err)
	setVersion := func(version string) {
		wrapped.requestsMu.Lock()
		wrapped.healthAPIVersion = version
		wrapped.requestsMu.Unlock()
	}
	move := func(args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		args["ref"] = source.Name + "#" + issue.ShortID
		args["to_project"] = target.Name
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: args})
		require.NoError(t, err)
		return result
	}
	setVersion("0.34.0")
	result := move(map[string]any{})
	require.True(t, result.IsError, string(mustJSON(t, result)))
	require.Contains(t, string(mustJSON(t, result)), "single")
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)

	setVersion("")
	result = move(map[string]any{"single": true})
	require.False(t, result.IsError, string(mustJSON(t, result)))
	for id, expected := range map[int64]int64{issue.ID: target.ID, child.ID: source.ID} {
		got, err := store.IssueByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, expected, got.ProjectID)
	}
}

// Contract: a kata.move preview never shows the issue first. Showing an issue
// on a federation spoke refreshes and persists claim status, so a preview must
// take the issue identity from the move validation response, as the CLI does.
func TestMoveToolPreviewDoesNotShowIssue(t *testing.T) {
	_, wrapped, session, source, target, _, issue := mcpFixedScopeFixture(t)
	wrapped.requestsMu.Lock()
	wrapped.requests = nil
	wrapped.requestsMu.Unlock()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID, "to_project": target.Name, "dry_run": true}})
	require.NoError(t, err)
	require.False(t, result.IsError, string(mustJSON(t, result)))
	var output MoveOutput
	require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
	require.Equal(t, issue.UID, output.Issue.UID)
	require.Equal(t, source.UID, output.From.UID)
	wrapped.requestsMu.Lock()
	defer wrapped.requestsMu.Unlock()
	for _, request := range wrapped.requests {
		require.NotRegexp(t, `^GET /api/v1/projects/\d+/issues/[^/]+$`, request, "preview must not show the issue")
	}
}

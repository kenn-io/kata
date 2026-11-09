package mcpserver

import (
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func historicalOutputMove(t *testing.T, store db.Storage, source, target db.Project, issue db.Issue) (db.Issue, db.Issue) {
	t.Helper()
	collisionChar := "0"
	if issue.UID[21] == '0' {
		collisionChar = "1"
	}
	collisionUID := issue.UID[:21] + collisionChar + issue.UID[22:]
	collision, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, UID: collisionUID, Title: "Unrelated target", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, issue.ShortID, collision.ShortID)
	moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotEqual(t, issue.ShortID, moved.Issue.ShortID, "collision forces distinct current and historical references")
	return moved.Issue, collision
}

func TestHistoricalMCPOutputsUseCurrentProject(t *testing.T) {
	for _, tool := range []string{"kata.show", "kata.edit", "kata.comment", "kata.edit_comment", "kata.set_label", "kata.restore"} {
		t.Run(tool, func(t *testing.T) {
			store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
			moved, collision := historicalOutputMove(t, store, source, target, issue)
			args := map[string]any{"ref": source.Name + "#" + issue.ShortID}
			switch tool {
			case "kata.edit":
				args["title"] = "Updated after move"
			case "kata.comment":
				args["body"] = "Continue in destination"
				args["idempotency_key"] = "historical-output-comment"
			case "kata.edit_comment":
				comment, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Original note"})
				require.NoError(t, err)
				args["comment_ref"] = comment.UID
				args["body"] = "Updated note"
			case "kata.set_label":
				args["label"] = "example-label"
				args["present"] = true
			case "kata.restore":
				_, _, _, err := store.SoftDeleteIssue(t.Context(), issue.ID, "worker")
				require.NoError(t, err)
			}
			output := callWorkflowTool(t, session, tool, args)
			project := output["project"].(map[string]any)
			require.Equal(t, target.Name, project["name"])
			require.Equal(t, target.UID, project["uid"])
			summary := output["issue"].(map[string]any)
			canonical := target.Name + "#" + moved.ShortID
			require.Equal(t, canonical, summary["qualified_ref"])
			shown := callWorkflowTool(t, session, "kata.show", map[string]any{"ref": canonical})
			require.Equal(t, issue.UID, shown["issue"].(map[string]any)["uid"])
			untouched, err := store.IssueByID(t.Context(), collision.ID)
			require.NoError(t, err)
			require.Equal(t, collision.Title, untouched.Title)
		})
	}
}

func TestHistoricalMCPDestructiveConfirmationUsesCurrentProject(t *testing.T) {
	for _, tool := range []string{"kata.delete", "kata.purge"} {
		t.Run(tool, func(t *testing.T) {
			store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
			moved, collision := historicalOutputMove(t, store, source, target, issue)
			verb := "DELETE"
			if tool == "kata.purge" {
				verb = "PURGE"
				_, _, _, err := store.SoftDeleteIssue(t.Context(), issue.ID, "worker")
				require.NoError(t, err)
			}
			canonical := target.Name + "#" + moved.ShortID
			args := map[string]any{"ref": source.Name + "#" + issue.ShortID, "confirm": verb + " " + source.Name + "#" + moved.ShortID}
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			refused, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
			require.NoError(t, err)
			require.True(t, refused.IsError)
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "rejected confirmation must not mutate the issue")
			_, err = store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)

			args["confirm"] = verb + " " + canonical
			output := callWorkflowTool(t, session, tool, args)
			require.Equal(t, target.Name, output["project"].(map[string]any)["name"])
			if tool == "kata.delete" {
				require.Equal(t, canonical, output["issue"].(map[string]any)["qualified_ref"])
			} else {
				require.Equal(t, canonical, output["ref"])
			}
			untouched, err := store.IssueByID(t.Context(), collision.ID)
			require.NoError(t, err)
			require.Equal(t, collision.Title, untouched.Title)
		})
	}
}

func TestHistoricalMCPDestructiveConfirmationCannotCrossStartupScope(t *testing.T) {
	for _, tool := range []string{"kata.delete", "kata.purge"} {
		t.Run(tool, func(t *testing.T) {
			store, _, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
			mcpMoveOutside(t, store, source, excluded, issue)
			verb := "DELETE"
			if tool == "kata.purge" {
				verb = "PURGE"
				_, _, _, err := store.SoftDeleteIssue(t.Context(), issue.ID, "worker")
				require.NoError(t, err)
			}
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID, "confirm": verb + " " + excluded.Name + "#" + issue.ShortID}})
			require.NoError(t, err)
			require.True(t, result.IsError)
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "scope denial must not mutate the issue")
			_, err = store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err, "out-of-scope issue must not be purged")
		})
	}
}

func TestHistoricalMCPWaitUsesCurrentReference(t *testing.T) {
	store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
	moved, _ := historicalOutputMove(t, store, source, target, issue)
	output := callWorkflowTool(t, session, "kata.wait", map[string]any{"refs": []string{source.Name + "#" + issue.ShortID}, "status": "open", "timeout_seconds": 1})
	state := output["states"].([]any)[0].(map[string]any)
	require.Equal(t, target.Name+"#"+moved.ShortID, state["ref"])
}

func TestHistoricalMCPStatusUsesCurrentReference(t *testing.T) {
	store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
	moved, _ := historicalOutputMove(t, store, source, target, issue)
	output := callWorkflowTool(t, session, "kata.status", map[string]any{"ref": source.Name + "#" + issue.ShortID})
	require.Equal(t, target.Name, output["project"].(map[string]any)["name"])
	require.Equal(t, target.Name+"#"+moved.ShortID, output["qualified_ref"])
}

func TestHistoricalMCPMoveUsesCurrentSourceIdentity(t *testing.T) {
	for _, mode := range []string{"automatic", "explicit", "stale"} {
		t.Run(mode, func(t *testing.T) {
			store, wrapped, _, source, current, destination, issue := mcpFixedScopeFixture(t)
			moved, collision := historicalOutputMove(t, store, source, current, issue)
			session := connectTestServerWithOptions(t, Options{Client: wrapped.client, Scope: NewAllScope(), Actor: "worker", Version: "test"})
			args := map[string]any{"ref": source.Name + "#" + issue.ShortID, "to_project": destination.Name}
			if mode == "explicit" {
				args["revision"] = moved.Revision
			}
			if mode == "stale" {
				args["revision"] = issue.Revision
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: args})
			require.NoError(t, err)
			if mode == "stale" {
				require.True(t, result.IsError, "caller revision must not be overwritten during preflight")
				got, err := store.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, current.ID, got.ProjectID)
			} else {
				require.False(t, result.IsError, string(mustJSON(t, result)))
				output := result.StructuredContent.(map[string]any)
				require.Equal(t, current.Name, output["from"].(map[string]any)["name"])
				got, err := store.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, destination.ID, got.ProjectID)
				require.Equal(t, destination.Name+"#"+got.ShortID, output["issue"].(map[string]any)["qualified_ref"])
			}
			untouched, err := store.IssueByID(t.Context(), collision.ID)
			require.NoError(t, err)
			require.Equal(t, current.ID, untouched.ProjectID)
		})
	}
}

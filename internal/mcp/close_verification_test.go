package mcpserver

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http/httptest"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPVerdictCloseAndWarnings(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "fixture.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	foreign, err := store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: project.ID, ProjectName: project.Name, Actor: "worker", Version: "test-version"})
	for _, kind := range []string{"confirmed", "refuted", "warning", "overrule"} {
		t.Run(kind, func(t *testing.T) {
			meta := map[string]jsontext.Value{}
			if kind == "confirmed" {
				meta["verify.required"] = jsontext.Value("true")
			}
			issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "worker", Title: "Completion claim", Metadata: meta})
			require.NoError(t, err)
			claim, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Completion claim"})
			require.NoError(t, err)
			replyKind := "refute"
			if kind == "confirmed" {
				replyKind = "confirm"
			}
			reviewIssue := issue
			if kind == "warning" || kind == "overrule" {
				reviewIssue, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "reviewer", Title: "Review"})
				require.NoError(t, err)
			}
			verdict, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: reviewIssue.ID, Author: "reviewer", Body: "Independent review", ReplyToUID: claim.UID, ReplyKind: replyKind})
			require.NoError(t, err)
			shown, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": issue.ShortID}})
			require.NoError(t, err)
			require.False(t, shown.IsError)
			require.Contains(t, shown.StructuredContent.(map[string]any), "verification")
			require.Equal(t, "legacy", shown.StructuredContent.(map[string]any)["issue"].(map[string]any)["verification"])
			reason := "done"
			evidence := []any{map[string]any{"type": "test", "command": "go test ./example"}}
			if kind == "confirmed" || kind == "refuted" {
				evidence = []any{map[string]any{"type": kind, "comment": claim.UID}}
			}
			if kind == "refuted" {
				reason = "wontfix"
			}
			args := map[string]any{"idempotency_key": kind + "-close", "ref": issue.ShortID, "reason": reason, "message": "Reviewed the requested behavior and verified the final completion decision.", "evidence": evidence}
			if kind == "overrule" {
				args["overrule"] = []string{verdict.UID}
			}
			closed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.close", Arguments: args})
			require.NoError(t, err)
			require.False(t, closed.IsError, closed.Content)
			output := closed.StructuredContent.(map[string]any)
			if kind == "warning" || kind == "overrule" {
				if kind == "warning" {
					require.Contains(t, output, "warnings")
				}
				if kind == "overrule" {
					before, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.audit_closes", Arguments: map[string]any{"until": "2100-01-01T00:00:00Z"}})
					require.NoError(t, err)
					require.False(t, before.IsError, before.Content)
					raw, err := json.Marshal(before.StructuredContent)
					require.NoError(t, err)
					require.Contains(t, string(raw), "overruled_refutations", "readable retained overrules must be included before the move")
				}
				current, err := store.IssueByID(t.Context(), reviewIssue.ID)
				require.NoError(t, err)
				_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: project.ID, ToProjectID: foreign.ID, IfMatchRev: current.Revision, Actor: "reviewer"})
				require.NoError(t, err)
				replay, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.close", Arguments: args})
				require.NoError(t, err)
				require.False(t, replay.IsError, replay.Content)
				raw, err := json.Marshal(replay.StructuredContent)
				require.NoError(t, err)
				require.NotContains(t, string(raw), "Independent review")
				require.NotContains(t, replay.StructuredContent.(map[string]any), "warnings")
				audit, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.audit_closes", Arguments: map[string]any{"until": "2100-01-01T00:00:00Z"}})
				require.NoError(t, err)
				require.False(t, audit.IsError, audit.Content)
				raw, err = json.Marshal(audit.StructuredContent)
				require.NoError(t, err)
				require.NotContains(t, string(raw), foreign.Name+"#", "audit references obey the same startup scope as replay warnings")
			} else {
				require.NotContains(t, output, "warnings")
			}
		})
	}
}

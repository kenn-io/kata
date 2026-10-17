package mcpserver

import (
	"encoding/json/v2"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestHistoricalRelationshipTargetsUseCurrentIncludedProject(t *testing.T) {
	for _, mode := range []string{"allowlist", "all"} {
		for _, operation := range []string{"create link", "add link", "remove link", "duplicate", "superseded"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				store, wrapped, session, source, target, _, peer := mcpFixedScopeFixture(t)
				if mode == "all" {
					session = connectTestServerWithOptions(t, Options{Client: wrapped.client, Scope: NewAllScope(), Actor: "worker", Version: "test"})
				}
				moved, _ := historicalOutputMove(t, store, source, target, peer)
				historicalRef := source.Name + "#" + peer.ShortID
				if operation == "create link" {
					output := callWorkflowTool(t, session, "kata.create", map[string]any{
						"project": source.Name, "title": "New related work", "related": []any{historicalRef}, "idempotency_key": "historical-related-create",
					})
					created, err := store.IssueByUID(t.Context(), output["issue"].(map[string]any)["uid"].(string), db.IncludeDeletedNo)
					require.NoError(t, err)
					links, err := store.LinksByIssue(t.Context(), created.ID)
					require.NoError(t, err)
					require.Len(t, links, 1)
					require.ElementsMatch(t, []int64{created.ID, moved.ID}, []int64{links[0].FromIssueID, links[0].ToIssueID})
					return
				}
				subject, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Operating issue", Author: "worker"})
				require.NoError(t, err)
				subjectRef := source.Name + "#" + subject.ShortID
				if operation == "add link" || operation == "remove link" {
					field := "add_related"
					if operation == "remove link" {
						callWorkflowTool(t, session, "kata.edit", map[string]any{"ref": subjectRef, "add_related": []any{target.Name + "#" + moved.ShortID}})
						field = "remove_related"
					}
					callWorkflowTool(t, session, "kata.edit", map[string]any{"ref": subjectRef, field: []any{historicalRef}})
					links, err := store.LinksByIssue(t.Context(), subject.ID)
					require.NoError(t, err)
					if operation == "remove link" {
						require.Empty(t, links)
					} else {
						require.Len(t, links, 1)
						require.ElementsMatch(t, []int64{subject.ID, moved.ID}, []int64{links[0].FromIssueID, links[0].ToIssueID})
					}
					return
				}
				evidenceType := "duplicate-of"
				if operation == "superseded" {
					evidenceType = "superseded-by"
				}
				callWorkflowTool(t, session, "kata.close", map[string]any{
					"ref": subjectRef, "reason": operation, "message": "Recorded the existing replacement after checking the moved issue identity.",
					"evidence": []any{map[string]any{"type": evidenceType, "issue_ref": historicalRef}},
				})
				closed, err := store.IssueByID(t.Context(), subject.ID)
				require.NoError(t, err)
				require.Equal(t, "closed", closed.Status)
				events, err := store.EventsAfter(t.Context(), db.EventsAfterParams{IssueUID: subject.UID, Types: []string{"issue.closed"}, Limit: 10})
				require.NoError(t, err)
				require.Len(t, events, 1)
				var payload struct {
					Evidence []struct {
						IssueRef string `json:"issue_ref"`
					} `json:"evidence"`
				}
				require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
				require.Len(t, payload.Evidence, 1)
				require.Equal(t, moved.UID, payload.Evidence[0].IssueRef)
			})
		}
	}
}

func TestHistoricalRelationshipTargetsRejectExcludedAndArchivedProjects(t *testing.T) {
	for _, destination := range []string{"excluded", "archived allowlist", "archived all"} {
		for _, operation := range []string{"add link", "duplicate"} {
			t.Run(destination+"/"+operation, func(t *testing.T) {
				store, wrapped, session, source, target, excluded, peer := mcpFixedScopeFixture(t)
				if destination == "archived all" {
					session = connectTestServerWithOptions(t, Options{Client: wrapped.client, Scope: NewAllScope(), Actor: "worker", Version: "test"})
				}
				if destination == "excluded" {
					target = excluded
				}
				historicalOutputMove(t, store, source, target, peer)
				if destination != "excluded" {
					_, _, err := store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: target.ID, Actor: "worker", Force: true})
					require.NoError(t, err)
				}
				subject, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Operating issue", Author: "worker"})
				require.NoError(t, err)
				subjectRef := source.Name + "#" + subject.ShortID
				historicalRef := source.Name + "#" + peer.ShortID
				tool := "kata.edit"
				args := map[string]any{"ref": subjectRef, "add_related": []any{historicalRef}}
				if operation == "duplicate" {
					tool = "kata.close"
					args = map[string]any{
						"ref": subjectRef, "reason": "duplicate", "message": "Recorded the existing replacement after checking the moved issue identity.",
						"evidence": []any{map[string]any{"type": "duplicate-of", "issue_ref": historicalRef}},
					}
				}
				before, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
				require.NoError(t, err)
				require.True(t, result.IsError, string(mustJSON(t, result)))
				after, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				require.Equal(t, before, after, "rejected targets must not append mutation or audit events")
				current, err := store.IssueByID(t.Context(), subject.ID)
				require.NoError(t, err)
				require.Equal(t, "open", current.Status)
				links, err := store.LinksByIssue(t.Context(), subject.ID)
				require.NoError(t, err)
				require.Empty(t, links)
			})
		}
	}
}

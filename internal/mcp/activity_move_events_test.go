package mcpserver

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMultiProjectEventsDeduplicateMovesBeforePagination(t *testing.T) {
	for _, mode := range []string{"poll", "wait"} {
		t.Run(mode, func(t *testing.T) {
			store, _, session, source, target, excluded, issue := mcpFixedScopeFixture(t)
			beforeMove, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			moved, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
				IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
				IfMatchRev: issue.Revision, Actor: "worker",
			})
			require.NoError(t, err)
			require.NotNil(t, moved.Event)
			_, commented, err := store.CreateComment(t.Context(), db.CreateCommentParams{
				IssueID: issue.ID, Body: "Continue in the destination", Author: "worker",
			})
			require.NoError(t, err)
			_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: excluded.ID, Title: "Excluded event", Author: "worker",
			})
			require.NoError(t, err)
			_, created, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
				ProjectID: source.ID, Title: "Later source work", Author: "worker",
			})
			require.NoError(t, err)

			page := func(after int64) EventsOutput {
				t.Helper()
				result := callWorkflowTool(t, session, "kata.events", map[string]any{
					"mode": mode, "after": after, "limit": 2, "wait_seconds": 1,
				})
				var output EventsOutput
				require.NoError(t, json.Unmarshal(mustJSON(t, result), &output))
				return output
			}
			first := page(beforeMove)
			var firstUIDs []string
			for _, event := range first.Events {
				firstUIDs = append(firstUIDs, event.EventUID)
			}
			require.Equal(t, []string{moved.Event.UID, commented.UID}, firstUIDs,
				"the move appears in both included project feeds but must consume only one result slot")
			require.Equal(t, commented.ID, first.NextAfterID, "resume after the last returned unique event")

			second := page(first.NextAfterID)
			require.Len(t, second.Events, 1, "the limited first page must not lose the later source event or expose excluded work")
			require.Equal(t, created.UID, second.Events[0].EventUID)
			require.Equal(t, created.ID, second.NextAfterID)

			empty := page(second.NextAfterID)
			require.Empty(t, empty.Events)
			require.Equal(t, second.NextAfterID, empty.NextAfterID, "empty polls and waits preserve the resume cursor")
			require.Equal(t, mode == "wait", empty.TimedOut)
		})
	}
}

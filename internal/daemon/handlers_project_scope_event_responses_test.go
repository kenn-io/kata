package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestProjectScopedCloseMutationHidesRestrictedParentReference(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newProjectAccessFixture(t, store)
		child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: fixture.public.ID, Title: "Task with restricted parent", Author: "member",
		})
		require.NoError(t, err)
		_, err = store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: child.ID, ToIssueID: fixture.issue.ID,
			Type:   "parent",
			Author: "member",
		})
		require.NoError(t, err)

		status, _, body := fixture.request(t, "POST",
			fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", fixture.public.ID, child.ShortID),
			"nonmember", map[string]any{
				"actor": "nonmember", "reason": "done",
				"message":  "Completed task after confirming every requested check.",
				"evidence": []map[string]any{{"type": "test", "command": "go test ./..."}},
			}, nil)
		require.Equalf(t, 200, status, "response body: %s", body)
		var response struct {
			Issue   db.Issue  `json:"issue"`
			Event   *db.Event `json:"event"`
			Changed bool      `json:"changed"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.True(t, response.Changed, "the close mutation still commits")
		require.Nil(t, response.Event,
			"the response must follow event-read filtering when the close parent is restricted")
		require.NotContains(t, string(body), fixture.issue.UID,
			"the response must not disclose an inaccessible parent UID")
		require.NotContains(t, string(body), fixture.issue.ShortID,
			"the response must not disclose an inaccessible parent short ID")
		closed, err := store.IssueByID(t.Context(), child.ID)
		require.NoError(t, err)
		require.Equal(t, "closed", closed.Status,
			"filtering response event references must not undo or block the close")
	})
}

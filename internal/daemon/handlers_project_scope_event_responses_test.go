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

func TestProjectScopedCloseRetryHidesRestrictedEvidenceReference(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newProjectAccessFixture(t, store)
		source, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: fixture.public.ID, Title: "Duplicate request", Author: "member",
		})
		require.NoError(t, err)
		path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", fixture.public.ID, source.ShortID)
		headers := map[string]string{"Idempotency-Key": "close-duplicate-with-restricted-evidence"}
		body := map[string]any{
			"actor":          "member",
			"reason":         "duplicate",
			"message":        "This request duplicates the existing tracked issue.",
			"retry_protocol": "close-v1",
			"evidence": []map[string]any{{
				"type": "duplicate-of", "issue_ref": fixture.visible.ShortID,
			}},
		}

		status, _, responseBody := fixture.request(t, "POST", path, "member", body, headers)
		require.Equalf(t, 200, status, "first close response: %s", responseBody)
		var first struct {
			Event *db.Event `json:"event"`
		}
		require.NoError(t, json.Unmarshal(responseBody, &first))
		require.NotNil(t, first.Event)
		require.Contains(t, string(first.Event.Payload), fixture.visible.ShortID)

		target, err := store.IssueByID(t.Context(), fixture.visible.ID)
		require.NoError(t, err)
		_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
			IssueID: target.ID, FromProjectID: fixture.public.ID, ToProjectID: fixture.private.ID,
			IfMatchRev: target.Revision, Actor: "admin",
		})
		require.NoError(t, err)
		_, err = store.SetTeamMembership(t.Context(), fixture.team.UID, "member", false, "admin")
		require.NoError(t, err)

		status, _, responseBody = fixture.request(t, "POST", path, "member", body, headers)
		require.Equalf(t, 200, status, "idempotent close retry response: %s", responseBody)
		var retry struct {
			OriginalEvent *db.Event `json:"original_event"`
			Reused        bool      `json:"reused"`
		}
		require.NoError(t, json.Unmarshal(responseBody, &retry))
		require.True(t, retry.Reused)
		require.Nil(t, retry.OriginalEvent,
			"a receipt whose duplicate-of target is outside the current project scope must be omitted")
		require.NotContains(t, string(responseBody), fixture.visible.ShortID,
			"the inaccessible evidence reference must not be returned by the retry")
	})
}

func TestProjectScopedIssueShowOmitsRestrictedParent(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := newProjectAccessFixture(t, store)
		child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: fixture.public.ID, Title: "Visible child", Author: "member",
		})
		require.NoError(t, err)
		_, err = store.CreateLink(t.Context(), db.CreateLinkParams{
			FromIssueID: child.ID, ToIssueID: fixture.issue.ID, Type: "parent", Author: "member",
		})
		require.NoError(t, err)

		status, _, body := fixture.request(t, "GET",
			fmt.Sprintf("/api/v1/projects/%d/issues/%s", fixture.public.ID, child.ShortID),
			"nonmember", nil, nil)
		require.Equalf(t, 200, status, "showing a visible issue must omit its inaccessible parent: %s", body)
		var response struct {
			Parent *struct {
				UID         string `json:"uid"`
				ShortID     string `json:"short_id"`
				QualifiedID string `json:"qualified_id"`
			} `json:"parent"`
		}
		require.NoError(t, json.Unmarshal(body, &response))
		require.Nil(t, response.Parent, "an inaccessible parent must be omitted from the visible child")
		require.NotContains(t, string(body), fixture.issue.UID)
		require.NotContains(t, string(body), fixture.issue.ShortID)
		require.NotContains(t, string(body), projectAccessCanary)
	})
}

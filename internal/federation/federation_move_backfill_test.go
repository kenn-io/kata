package federation

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func TestFederationIssueHistoryRequiresCurrentProject(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "history-authorized", "history-excluded")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[1].ID, Title: "excluded issue", Author: "worker"})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/events?issue_uid=%s", hub.URL, projects[0].ID, issue.UID), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+spoke.credentials[projects[0].UID].Token)
	request.Header.Set("X-Kata-Event-Features", db.SupportedEventFeatures)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, http.StatusNotFound, response.StatusCode)
}

func TestFederationIssueHistoryRetainsEnvelopeAndPagination(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "history-original", "history-current")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	issue, creation, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moving history", Author: "worker"})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
	require.NoError(t, err)
	issue, err = hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	unrelated, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[1].ID, Title: "unrelated destination issue", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = hub.DB.SoftDeleteIssue(t.Context(), unrelated.ID, "worker")
	require.NoError(t, err)
	_, err = hub.DB.PurgeIssue(t.Context(), unrelated.ID, "worker", nil)
	require.NoError(t, err)
	var cursor int64
	var events []api.EventEnvelope
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/events?issue_uid=%s&after_id=%d&limit=1", hub.URL, projects[1].ID, issue.UID, cursor), nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+spoke.credentials[projects[1].UID].Token)
		request.Header.Set("X-Kata-Event-Features", db.SupportedEventFeatures)
		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		var page api.PollEventsBody
		err = json.UnmarshalRead(response.Body, &page)
		require.NoError(t, response.Body.Close())
		require.NoError(t, err)
		require.False(t, page.ResetRequired)
		if len(page.Events) == 0 {
			break
		}
		require.Len(t, page.Events, 1)
		require.Greater(t, page.NextAfterID, cursor)
		cursor = page.NextAfterID
		events = append(events, page.Events...)
		if len(events) > 10 {
			t.Fatal("history pagination failed to terminate")
		}
	}
	require.Len(t, events, 3)
	for _, event := range events {
		if event.IssueUID == nil {
			t.Fatal("history event has no subject")
		}
		require.Equal(t, issue.UID, *event.IssueUID)
	}
	require.Equal(t, creation.UID, events[0].EventUID)
	require.Equal(t, projects[0].UID, events[0].ProjectUID)
	require.Equal(t, creation.ContentHash, events[0].ContentHash)
}

func TestSyncFederationMoveRecoversHistoryBeforeDestinationCursor(t *testing.T) {
	for _, scenario := range []struct {
		name                string
		localEdit           bool
		leaveBeforeDelivery bool
	}{
		{name: "source_leave"},
		{name: "source_leave_before_move", leaveBeforeDelivery: true},
		{name: "preserve_newer_edit", localEdit: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "history-source", "history-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "initial title", Body: "initial body", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))

			title, body := "source title", "source body"
			issue, edit, _, err := hub.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &title, Body: &body, Actor: "worker"})
			require.NoError(t, err)
			if edit == nil {
				t.Fatal("missing source edit event")
			}
			_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "missed source discussion"})
			require.NoError(t, err)
			_, _, err = hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[1].ID, Title: "advance destination cursor", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 1))
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			require.Greater(t, binding.PullCursorEventID, edit.ID)
			if scenario.localEdit {
				local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				body = "newer independent body"
				_, _, _, err = spoke.env.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: local.ID, Body: &body, Actor: "worker"})
				require.NoError(t, err)
			}
			issue, err = hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			if !scenario.localEdit {
				require.NoError(t, spoke.credentials.DeleteFederationCredential(t.Context(), spoke.projects[0].UID))
			}
			if scenario.leaveBeforeDelivery {
				_, err = spoke.env.DB.LeaveFederationReplica(t.Context(), spoke.projects[0].ID)
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 1), "destination must recover history without requiring the source credential")
			if !scenario.localEdit {
				if !scenario.leaveBeforeDelivery {
					_, err = spoke.env.DB.LeaveFederationReplica(t.Context(), spoke.projects[0].ID)
					require.NoError(t, err)
				}
				require.NoError(t, spoke.sync(t, 1), "source teardown must not strand missing content")
			}
			got, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, spoke.projects[1].ID, got.ProjectID)
			require.Equal(t, title, got.Title)
			require.Equal(t, body, got.Body)
			comments, err := spoke.env.DB.CommentsByIssue(t.Context(), got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "missed source discussion", comments[0].Body)
		})
	}
}

package federation

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestSyncFederationMoveUsesBaselineAfterLocalMove(t *testing.T) {
	for _, joinOriginal := range []bool{true, false} {
		name := "original_project_joined"
		if !joinOriginal {
			name = "only_federated_projects_joined"
		}
		t.Run(name, func(t *testing.T) {
			hub := testenv.New(t)
			projects := make([]db.Project, 3)
			for i, projectName := range []string{"local-origin", "baseline-source", "baseline-target"} {
				var err error
				projects[i], err = hub.DB.CreateProject(t.Context(), projectName)
				require.NoError(t, err)
			}
			// Enable the eventual destination first so its ordinary poll page,
			// as well as the independent backfill, includes the old local move.
			_, err := hub.DB.EnableProjectFederation(t.Context(), projects[2].ID, "worker")
			require.NoError(t, err)
			issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "local title", Body: "local body", Author: "worker"})
			require.NoError(t, err)
			_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "before federation"})
			require.NoError(t, err)
			issue, err = hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = hub.DB.EnableProjectFederation(t.Context(), projects[1].ID, "worker")
			require.NoError(t, err)
			joined := projects[1:]
			if joinOriginal {
				_, err = hub.DB.EnableProjectFederation(t.Context(), projects[0].ID, "worker")
				require.NoError(t, err)
				joined = projects
			}
			spoke := newMoveDeliverySpoke(t, hub, joined)
			title, body := "after baseline title", "after baseline body"
			_, _, _, err = hub.DB.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &title, Body: &body, Actor: "worker"})
			require.NoError(t, err)
			_, _, err = hub.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "after federation"})
			require.NoError(t, err)
			issue, err = hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			target := len(joined) - 1
			require.NoError(t, spoke.sync(t, target), "the federation baseline supersedes the snapshotless local move")
			got, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, spoke.projects[target].ID, got.ProjectID)
			require.Equal(t, title, got.Title)
			require.Equal(t, body, got.Body)
			comments, err := spoke.env.DB.CommentsByIssue(t.Context(), got.ID)
			require.NoError(t, err)
			bodies := make([]string, 0, len(comments))
			for _, comment := range comments {
				bodies = append(bodies, comment.Body)
			}
			require.ElementsMatch(t, []string{"before federation", "after federation"}, bodies)
			if !joinOriginal {
				_, err = spoke.env.DB.ProjectByUID(t.Context(), projects[0].UID)
				require.ErrorIs(t, err, db.ErrNotFound)
			}
		})
	}
}

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

func TestSyncFederationConcurrentMoveRecoversMissedSourceHistory(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "branch-history-source", "branch-history-middle", "branch-history-target")
	receiver := newMoveDeliverySpoke(t, hub, projects)
	authors := []moveDeliverySpoke{newMoveDeliverySpoke(t, hub, projects), newMoveDeliverySpoke(t, hub, projects)}
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "initial title", Body: "initial body", Author: "worker"})
	require.NoError(t, err)
	for _, spoke := range append(authors, receiver) {
		for i := range projects {
			require.NoError(t, spoke.sync(t, i))
		}
	}

	title, body := "missed source title", "missed source body"
	_, edit, _, err := hub.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Body: &body, Actor: "worker"})
	require.NoError(t, err)
	if edit == nil {
		t.Fatal("missing source edit event")
	}
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "missed source discussion"})
	require.NoError(t, err)
	for _, author := range authors {
		require.NoError(t, author.sync(t, 0))
	}
	for destination := 1; destination < len(projects); destination++ {
		_, _, err = hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[destination].ID, Title: "advance destination cursor", Author: "worker"})
		require.NoError(t, err)
		require.NoError(t, receiver.sync(t, destination))
		binding, err := receiver.env.DB.FederationBindingByProject(ctx, receiver.projects[destination].ID)
		require.NoError(t, err)
		require.Greater(t, binding.PullCursorEventID, edit.ID)
	}

	var moves []db.FoldEvent
	for index, author := range authors {
		local, err := author.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		moved, err := author.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: author.projects[0].ID, ToProjectID: author.projects[index+1].ID, IfMatchRev: local.Revision, Actor: "worker"})
		require.NoError(t, err)
		if moved.Event == nil {
			t.Fatal("missing concurrent move event")
		}
		event := moved.Event
		moves = append(moves, db.FoldEvent{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: projects[index+1].UID, IssueUID: issue.UID, Type: event.Type, Actor: event.Actor, Payload: []byte(event.Payload), HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter})
	}
	winningProject := db.FoldEvents(moves).Issues[issue.UID].ProjectUID
	winner, loser := 0, 1
	if winningProject == projects[2].UID {
		winner, loser = 1, 0
	}
	require.NoError(t, authors[winner].sync(t, winner+1))
	require.NoError(t, authors[loser].sync(t, loser+1), "the losing move arrives last")
	current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[winner+1].ID, current.ProjectID)

	_, err = receiver.env.DB.LeaveFederationReplica(ctx, receiver.projects[0].ID)
	require.NoError(t, err)
	require.NoError(t, receiver.credentials.DeleteFederationCredential(ctx, receiver.projects[0].UID))
	require.NoError(t, receiver.sync(t, winner+1))
	require.NoError(t, receiver.sync(t, winner+1), "ordinary retry must retain the recovered history after source leave")
	got, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, receiver.projects[winner+1].ID, got.ProjectID)
	require.Equal(t, title, got.Title)
	require.Equal(t, body, got.Body)
	comments, err := receiver.env.DB.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "missed source discussion", comments[0].Body)
}

func TestSyncFederationMoveUsesReenrollmentBaselineAfterLocalMove(t *testing.T) {
	ctx := t.Context()
	oldHub, original := newMoveDeliveryHub(t, "old-enrollment-source")
	issue, _, err := oldHub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: original[0].ID, Title: "old enrollment title", Author: "worker"})
	require.NoError(t, err)
	_, _, err = oldHub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "old enrollment discussion"})
	require.NoError(t, err)
	_, _, err = oldHub.DB.RefreshProjectFederationBaseline(ctx, original[0].ID, "worker")
	require.NoError(t, err)
	newHub := newMoveDeliverySpoke(t, oldHub, original)
	require.NoError(t, newHub.sync(t, 0))
	_, err = newHub.env.DB.LeaveFederationReplica(ctx, newHub.projects[0].ID)
	require.NoError(t, err)
	projects := make([]db.Project, 2)
	for i, name := range []string{"reenrollment-source", "reenrollment-target"} {
		projects[i], err = newHub.env.DB.CreateProject(ctx, name)
		require.NoError(t, err)
	}
	local, err := newHub.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = newHub.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: local.ProjectID, ToProjectID: projects[0].ID, IfMatchRev: local.Revision, Actor: "worker"})
	require.NoError(t, err)
	for _, project := range projects {
		_, err = newHub.env.DB.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	title := "after reenrollment title"
	_, _, _, err = newHub.env.DB.EditIssue(ctx, db.EditIssueParams{IssueID: local.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	_, _, err = newHub.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "after reenrollment discussion"})
	require.NoError(t, err)
	local, err = newHub.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = newHub.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: local.Revision, Actor: "worker"})
	require.NoError(t, err)
	receiver := newMoveDeliverySpoke(t, newHub.env, projects)
	require.NoError(t, receiver.sync(t, 1), "the later enrollment baseline covers the intervening snapshotless standalone move")
	got, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, receiver.projects[1].ID, got.ProjectID)
	require.Equal(t, title, got.Title, "post-baseline deltas remain visible")
	comments, err := receiver.env.DB.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	var bodies []string
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	require.ElementsMatch(t, []string{"old enrollment discussion", "after reenrollment discussion"}, bodies)
	_, err = receiver.env.DB.ProjectByUID(ctx, original[0].UID)
	require.ErrorIs(t, err, db.ErrNotFound, "covered prior enrollment identities need not be joined")
}

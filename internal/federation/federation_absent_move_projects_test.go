package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
)

// A direct spoke must be able to consume B's history without local A or C rows.
func TestSyncFederationMoveHistoryWithAbsentCounterpartProjects(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "subscribed-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[1:2])

	inbound, created, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Inbound issue", Author: "worker"})
	require.NoError(t, err)
	inMove, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: inbound.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: inbound.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, inMove.Event)
	outbound, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Outbound issue", Author: "worker"})
	require.NoError(t, err)
	outMove, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: outbound.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: outbound.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, outMove.Event)
	later, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Later work", Author: "worker"})
	require.NoError(t, err)
	for _, project := range []db.Project{projects[0], projects[2]} {
		_, err = spoke.env.DB.ProjectByUID(ctx, project.UID)
		require.ErrorIs(t, err, db.ErrNotFound)
	}

	client, err := NewClient(ctx, hub.URL, spoke.credentials[projects[1].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	page, err := client.PollProjectEvents(ctx, projects[1].ID, 0, 100)
	require.NoError(t, err)
	require.False(t, page.ResetRequired)
	history, err := backfillFederationMoveHistory(ctx, client, projects[1].ID, page)
	require.NoError(t, err)
	var historyUIDs []string
	for _, event := range history {
		historyUIDs = append(historyUIDs, event.EventUID)
	}
	require.Contains(t, historyUIDs, created.UID, "the inbound creation must be in the retained history")
	require.Contains(t, historyUIDs, inMove.Event.UID)
	require.Contains(t, historyUIDs, outMove.Event.UID)
	require.Contains(t, historyUIDs, laterEvent.UID)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	var delivered []string
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			if event.Type != db.ProjectScopeResetEventType {
				delivered = append(delivered, event.UID)
			}
		}
	}, spoke.credentials), "missing counterpart project rows must not stop the pull")
	require.Equal(t, historyUIDs, delivered, "delivery preserves the hub history order")
	got, err := spoke.env.DB.IssueByUID(ctx, inbound.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, spoke.projects[0].ID, got.ProjectID)
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "later B work must be visible")
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
	for _, project := range []db.Project{projects[0], projects[2]} {
		retained, err := spoke.env.DB.ProjectByUID(ctx, project.UID)
		require.NoError(t, err)
		require.NotNil(t, retained.DeletedAt)
		_, err = spoke.env.DB.FederationBindingByProject(ctx, retained.ID)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	for _, original := range []db.Event{created, *inMove.Event, *outMove.Event, laterEvent} {
		require.NotNil(t, original.IssueUID)
		stored, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: *original.IssueUID, Limit: 100})
		require.NoError(t, err)
		var found bool
		for _, event := range stored {
			if event.UID == original.UID {
				found = true
				require.Equal(t, original.ProjectUID, event.ProjectUID)
				require.Equal(t, original.OriginInstanceUID, event.OriginInstanceUID)
				require.Equal(t, original.ContentHash, event.ContentHash)
				require.Equal(t, original.Actor, event.Actor)
				require.JSONEq(t, original.Payload, event.Payload)
			}
		}
		require.True(t, found, "event %s must retain its original identity", original.UID)
	}
}

// A previously visible issue must leave the subscribed project's scope.
func TestSyncFederationMoveOutToAbsentProject(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "subscribed-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Outbound issue", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	target, err := spoke.env.DB.ProjectByUID(ctx, projects[1].UID)
	require.NoError(t, err)
	require.NotNil(t, target.DeletedAt)
	_, err = spoke.env.DB.FederationBindingByProject(ctx, target.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	title := "Updated after move"
	_, _, _, err = hub.DB.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Title: &title, Actor: "worker"})
	require.NoError(t, err)
	_, _, _, err = spoke.env.DB.RestoreProject(ctx, target.ID, "worker")
	require.NoError(t, err)
	_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestSyncFederationMoveOutAfterPushedEdit(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "operator-archive"}[archived], func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "subscribed-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects[:1])
			if archived {
				target, err := spoke.env.DB.CreateProjectWithUID(ctx, "target-project", projects[1].UID)
				require.NoError(t, err)
				_, _, err = spoke.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: target.ID, Actor: "worker"})
				require.NoError(t, err)
			}
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Initial", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			title := "Edited on spoke"
			_, edit, _, err := spoke.env.DB.EditIssue(ctx, db.EditIssueParams{IssueID: local.ID, Title: &title, Actor: "worker"})
			require.NoError(t, err)
			require.NotNil(t, edit.IssueID)
			require.NoError(t, spoke.sync(t, 0))
			current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, title, current.Title)
			moved, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: current.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			require.GreaterOrEqual(t, binding.PullCursorEventID, moved.Event.ID)
			listed, err := spoke.env.DB.ListIssues(ctx, db.ListIssuesParams{ProjectID: spoke.projects[0].ID, Limit: 100})
			require.NoError(t, err)
			require.Empty(t, listed)
			_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			history, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: issue.UID, Limit: 100})
			require.NoError(t, err)
			var retained bool
			for _, event := range history {
				if event.UID == edit.UID {
					retained = true
					require.Equal(t, edit.ContentHash, event.ContentHash)
					require.Equal(t, edit.ProjectUID, event.ProjectUID)
					require.Equal(t, edit.IssueUID, event.IssueUID)
					require.JSONEq(t, edit.Payload, event.Payload)
				}
			}
			require.True(t, retained)
			current, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[1].ID, ToProjectID: projects[0].ID, IfMatchRev: current.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			restored, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, spoke.projects[0].ID, restored.ProjectID)
			require.Equal(t, title, restored.Title)
		})
	}
}

func TestSyncFederationMoveOutToOperatorArchive(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "subscribed-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	target, err := spoke.env.DB.CreateProjectWithUID(ctx, "target-project", projects[1].UID)
	require.NoError(t, err)
	_, _, err = spoke.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: target.ID, Actor: "worker"})
	require.NoError(t, err)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Issue", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	restored, _, _, err := spoke.env.DB.RestoreProject(ctx, target.ID, "worker")
	require.NoError(t, err)
	require.Equal(t, "target-project", restored.Name)
	_, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
}

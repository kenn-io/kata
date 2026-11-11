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
	outbound, outboundCreated, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Outbound issue", Author: "worker"})
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

	expected := []string{created.UID, inMove.Event.UID, outboundCreated.UID, outMove.Event.UID, laterEvent.UID}
	expectedSet := make(map[string]bool, len(expected))
	for _, eventUID := range expected {
		expectedSet[eventUID] = true
	}

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	var delivered []string
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			if expectedSet[event.UID] {
				delivered = append(delivered, event.UID)
			}
		}
	}, spoke.credentials), "missing counterpart project rows must not stop the pull")
	require.Equal(t, expected, delivered, "delivery preserves the hub history order")
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "later B work must be visible")
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
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
			current, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[1].ID, ToProjectID: projects[0].ID, IfMatchRev: current.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			restored, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, spoke.projects[0].ID, restored.ProjectID)
			require.Equal(t, title, restored.Title)
			if archived {
				target, err := spoke.env.DB.ProjectByUID(ctx, projects[1].UID)
				require.NoError(t, err)
				reopened, _, _, err := spoke.env.DB.RestoreProject(ctx, target.ID, "worker")
				require.NoError(t, err)
				require.Equal(t, "target-project", reopened.Name)
			}
		})
	}
}

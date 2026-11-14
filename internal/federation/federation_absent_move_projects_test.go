package federation

import (
	"slices"
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
	_, laterEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Later work", Author: "worker"})
	require.NoError(t, err)

	expected := []string{created.UID, inMove.Event.UID, outboundCreated.UID, outMove.Event.UID, laterEvent.UID}
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	var delivered []string
	require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[1].UID], clientpkg.Opts{}, func(_ int64, events []db.Event) {
		for _, event := range events {
			if slices.Contains(expected, event.UID) {
				delivered = append(delivered, event.UID)
			}
		}
	}, spoke.credentials), "missing counterpart project rows must not stop the pull")
	require.Equal(t, expected, delivered, "delivery preserves the hub history order")
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PullCursorEventID, laterEvent.ID)
}

package federation

import (
	"testing"
	"time"

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

func TestSyncFederationMoveApplyErrorContext(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "error-source", "error-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Claimed issue", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	now := time.Now().UTC()
	root, _, err := spoke.env.DB.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: spoke.projects[0].ID, IssueID: local.ID, ConnectorInstance: "notes", ExternalRootKey: "root", ExternalAccountKey: "account", Actor: "worker", ReceiveCommentsAfter: now})
	require.NoError(t, err)
	_, claimed, err := spoke.env.DB.ClaimExternalRootBinding(ctx, root.ID, "worker-token", now, now.Add(-db.ExternalRootClaimStaleAfter))
	require.NoError(t, err)
	require.True(t, claimed)
	moved, err := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	err = SyncFederationPullOnlyOnce(ctx, spoke.env.DB, binding, spoke.credentials[projects[0].UID], spoke.credentials)
	require.ErrorIs(t, err, db.ErrExternalRootClaimActive)
	require.ErrorContains(t, err, "apply pull page")
	require.ErrorContains(t, err, moved.Event.ProjectUID)
	require.ErrorContains(t, err, moved.Event.UID)
}

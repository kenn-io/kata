package federation

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A hub project was archived and replaced. Pushing the same issues into the
// replacement can never succeed while the archived project holds their UIDs,
// so the spoke must stop and show the reason instead of retrying silently.
func TestSyncFederationIssueHeldByArchivedProjectQuarantinesPush(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "hub-project", "replacement-project")
	first := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := first.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: first.projects[0].ID, Title: "held task", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, first.sync(t, 0))
	_, _, err = hub.DB.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: projects[0].ID, Actor: "worker", Force: true})
	require.NoError(t, err)

	second := newMoveDeliverySpoke(t, hub, projects[1:])
	_, _, err = second.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: second.projects[0].ID, UID: issue.UID, Title: "held task", Author: "worker"})
	require.NoError(t, err)

	err = second.sync(t, 0)
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusBadRequest, status.StatusCode)
	require.Contains(t, status.Body, issue.UID)
	require.Contains(t, status.Body, "another project on this hub")

	quarantine, err := second.env.DB.ActiveFederationQuarantine(t.Context(), second.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err, "the refused push is quarantined, not retried forever")
	require.Contains(t, quarantine.Error, "another project on this hub")
	syncStatus, err := second.env.DB.FederationSyncStatusByProject(t.Context(), second.projects[0].ID)
	require.NoError(t, err)
	require.NotNil(t, syncStatus.LastError, "federation status shows the reason")
	require.Contains(t, *syncStatus.LastError, issue.UID)

	require.ErrorIs(t, second.sync(t, 0), ErrFederationPushQuarantined, "the next cycle stops before the network")
}

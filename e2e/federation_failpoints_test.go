//go:build federation_stress && !windows

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationFailpointHubRestartMidIngestBatch(t *testing.T) {
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_federation_ingest_commit=exit")
	fx := newFederationStressFixture(t, 1)
	fx.enableProject(t, "failpoint-mid-ingest")

	spokeIssue := fx.createIssueOnNode(t, &fx.spokes[0], fx.spokes[0].replica.Project.ID, "precommit-crash")
	fx.waitForHubUnavailable(t)
	_, err := fx.hub.db.IssueByUID(context.Background(), spokeIssue.UID, db.IncludeDeletedYes)
	require.Error(t, err, "pre-commit crash must not leave a partially-ingested issue")

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	fx.restartHub(t)
	fx.waitForConvergence(t)
	got, err := fx.hub.db.IssueByUID(context.Background(), spokeIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, spokeIssue.ShortID, got.ShortID)
}

func TestFederationFailpointHubCommitBeforeBroadcastStillConverges(t *testing.T) {
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "after_federation_ingest_commit_before_broadcast=exit")
	fx := newFederationStressFixture(t, 2)
	fx.enableProject(t, "failpoint-post-commit")

	spokeIssue := fx.createIssueOnNode(t, &fx.spokes[0], fx.spokes[0].replica.Project.ID, "postcommit-crash")
	fx.waitForHubUnavailable(t)
	got, err := fx.hub.db.IssueByUID(context.Background(), spokeIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "post-commit crash must retain the accepted issue")
	assert.Equal(t, spokeIssue.ShortID, got.ShortID)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	fx.restartHub(t)
	fx.waitForConvergence(t)
	fx.assertHubEventCountForIssue(t, spokeIssue.UID, 1)
	waitForFederatedIssue(t, fx.spokes[1].db, spokeIssue.UID, fx.spokes[1].stderr)
}

func TestFederationFailpointBeforeSpokePushCursorAdvanceRetriesDuplicateBatch(t *testing.T) {
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=exit")
	fx := newFederationStressFixture(t, 1)
	fx.enableProject(t, "failpoint-push-cursor")

	spokeIssue := fx.createIssueOnNode(t, &fx.spokes[0], fx.spokes[0].replica.Project.ID, "cursor-crash")
	fx.waitForSpokeUnavailable(t, 0)
	fx.assertHubEventCountForIssue(t, spokeIssue.UID, 1)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	fx.restartSpoke(t, 0)
	fx.waitForConvergence(t)
	fx.assertHubEventCountForIssue(t, spokeIssue.UID, 1)
	binding, err := fx.spokes[0].db.FederationBindingByProject(context.Background(), fx.spokes[0].replica.Project.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, binding.PushCursorEventID, int64(1))
}

func TestFederationFailpointConcurrentLocalWriteDuringPullApply(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pull-apply-sleep")
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS",
		"during_spoke_pull_apply_before_materialize=sleep:500ms:mark:"+marker)
	fx := newFederationStressFixture(t, 1)
	fx.enableProject(t, "failpoint-pull-apply")
	fx.waitForConvergence(t)
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		require.NoError(t, err)
	}

	hubIssue := fx.createIssueOnNode(t, &fx.hub, fx.hubProject.ID, "hub-pull-sleep")
	fx.waitForFailpointMarker(t, marker)
	localIssue := fx.createIssueOnNode(t, &fx.spokes[0], fx.spokes[0].replica.Project.ID, "local-during-pull")
	fx.waitForConvergence(t)

	waitForFederatedIssue(t, fx.spokes[0].db, hubIssue.UID, fx.spokes[0].stderr)
	waitForFederatedIssue(t, fx.hub.db, localIssue.UID, fx.hub.stderr)
	fx.assertEventOrderValid(t, fx.spokes[0].db, fx.spokes[0].replica.Project.ID)
	fx.assertDaemonStderrClean(t)
}

func TestFederationFailpointSpokeRestartDuringPendingPush(t *testing.T) {
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=exit")
	fx := newFederationStressFixture(t, 1)
	fx.enableProject(t, "failpoint-restart-pending-push")

	spokeIssue := fx.createIssueOnNode(t, &fx.spokes[0], fx.spokes[0].replica.Project.ID, "restart-pending-push")
	fx.waitForSpokeUnavailable(t, 0)

	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	fx.restartSpoke(t, 0)
	fx.waitForConvergence(t)
	waitForFederatedIssue(t, fx.hub.db, spokeIssue.UID, fx.hub.stderr)
	fx.assertHubEventCountForIssue(t, spokeIssue.UID, 1)
}

func TestFederationFailpointHubUnavailableClaimStatusFallsBackToCache(t *testing.T) {
	fx := newFederationStressFixture(t, 1)
	fx.enableProject(t, "failpoint-claim-status-offline")

	statusIssue := waitForFederatedIssue(t, fx.spokes[0].db, fx.hubIssue.UID, fx.spokes[0].stderr)
	now := time.Now().UTC()
	cached := db.IssueClaim{
		ClaimUID:          "01KSD7P0000000000000000001",
		ProjectID:         fx.spokes[0].replica.Project.ID,
		IssueID:           statusIssue.ID,
		IssueUID:          statusIssue.UID,
		Holder:            "cached-holder",
		HolderInstanceUID: fx.spokes[0].db.InstanceUID(),
		ClientKind:        "stress",
		Purpose:           "edit",
		ClaimKind:         "hard",
		AcquiredAt:        now,
		Revision:          1,
		UpdatedAt:         now,
	}
	require.NoError(t, fx.spokes[0].db.ApplyClaimStatus(context.Background(),
		fx.spokes[0].replica.Project.ID, statusIssue.UID, db.ClaimStatus{
			Held: true,
			Holder: db.ClaimPrincipal{
				Holder:            cached.Holder,
				HolderInstanceUID: cached.HolderInstanceUID,
				ClientKind:        cached.ClientKind,
			},
			Claim:  &cached,
			HubNow: now,
		}))
	fx.stopHub(t)

	for i := 0; i < 2; i++ {
		status, raw := stressDoJSON(t, fx.spokes[0].http, http.MethodGet,
			fx.spokes[0].url+"/api/v1/projects/"+strconv.FormatInt(fx.spokes[0].replica.Project.ID, 10)+
				"/issues/"+statusIssue.ShortID,
			nil, nil, nil)
		require.Equalf(t, http.StatusOK, status, "show body: %s", raw)
		require.Contains(t, string(raw), "cached-holder")
	}
}

func (fx *federationStressFixture) waitForFailpointMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("failpoint marker %s was not written", path)
}

func (fx *federationStressFixture) assertHubEventCountForIssue(t *testing.T, issueUID string, want int) {
	t.Helper()
	var got int
	require.NoError(t, fx.hub.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE project_id = ? AND issue_uid = ?`,
		fx.hubProject.ID, issueUID).Scan(&got))
	assert.Equal(t, want, got, fmt.Sprintf("hub event count for issue %s", issueUID))
}

func (fx *federationStressFixture) waitForHubUnavailable(t *testing.T) {
	t.Helper()
	fx.waitForHTTPUnavailable(t, fx.hub.http, fx.hub.url)
	fx.hub.online = false
}

func (fx *federationStressFixture) stopHub(t *testing.T) {
	t.Helper()
	stopDaemon(fx.hub.cmd)
	fx.waitForHTTPUnavailable(t, fx.hub.http, fx.hub.url)
	fx.hub.online = false
}

func (fx *federationStressFixture) waitForSpokeUnavailable(t *testing.T, i int) {
	t.Helper()
	fx.waitForHTTPUnavailable(t, fx.spokes[i].http, fx.spokes[i].url)
	fx.spokes[i].online = false
}

func (fx *federationStressFixture) waitForHTTPUnavailable(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/api/v1/ping") //nolint:noctx
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon at %s still answered /api/v1/ping", base)
}

func (fx *federationStressFixture) restartHub(t *testing.T) {
	t.Helper()
	stopDaemon(fx.hub.cmd)
	addr := strings.TrimPrefix(fx.hub.url, "http://")
	fx.hub.cmd = startFederationStressTCPDaemon(t, fx.bin, fx.hub.dirs, fx.hub.stderr, addr)
	stressWaitForPing(t, fx.hub.url, 5*time.Second)
	fx.hub.online = true
}

func (fx *federationStressFixture) restartSpoke(t *testing.T, i int) {
	t.Helper()
	stopDaemon(fx.spokes[i].cmd)
	fx.spokes[i].cmd = startFederationStressUnixDaemon(t, fx.bin, fx.spokes[i].dirs, fx.spokes[i].stderr)
	fx.spokes[i].url, fx.spokes[i].http = stressConnectDaemon(t, fx.spokes[i].dirs, fx.spokes[i].stderr)
	fx.spokes[i].online = true
}

func (fx *federationStressFixture) assertEventOrderValid(t *testing.T, store *sqlitestore.Store, projectID int64) {
	t.Helper()
	rows, err := store.QueryContext(context.Background(), `
		SELECT id, hlc_physical_ms, hlc_counter
		  FROM events
		 WHERE project_id = ?
		 ORDER BY id`, projectID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var lastID, lastPhysical, lastCounter int64
	for rows.Next() {
		var id, physical, counter int64
		require.NoError(t, rows.Scan(&id, &physical, &counter))
		assert.Greater(t, id, lastID)
		if lastPhysical != 0 {
			assert.Truef(t,
				physical > lastPhysical || (physical == lastPhysical && counter >= lastCounter),
				"event HLC order regressed at id %d: (%d,%d) after (%d,%d)",
				id, physical, counter, lastPhysical, lastCounter)
		}
		lastID, lastPhysical, lastCounter = id, physical, counter
	}
	require.NoError(t, rows.Err())
}

//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationPhase4OperationalRecovery(t *testing.T) {
	ctx := context.Background()
	fx := setupFederationClaimE2E(t, "phase4-operational")
	issueA := waitForFederatedIssue(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)
	_ = waitForFederatedIssue(t, fx.spokeB.db, fx.hubIssue.UID, fx.spokeB.stderr)
	survivor, _, err := fx.hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: fx.hubProject.ID,
		Title:     "survives purge reset",
		Body:      "must remain after another issue is purged",
		Author:    "agent",
	})
	require.NoError(t, err)
	waitForFederatedIssue(t, fx.spokeA.db, survivor.UID, fx.spokeA.stderr)
	waitForFederatedIssue(t, fx.spokeB.db, survivor.UID, fx.spokeB.stderr)

	status := runKataJSON[e2eFederationStatusBody](t, fx.bin, fx.spokeA.dirs,
		"federation", "status")
	require.Len(t, status.Statuses, 1)
	assert.Equal(t, int64(0), status.Statuses[0].ActiveQuarantineCount)
	assert.Empty(t, status.Statuses[0].ResetBlocker)

	q, err := fx.spokeA.db.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    fx.spokeA.replica.Project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7", "evt-8", "evt-9"},
		Error:        "operator test quarantine",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)

	status = runKataJSON[e2eFederationStatusBody](t, fx.bin, fx.spokeA.dirs,
		"federation", "status")
	require.Len(t, status.Statuses, 1)
	assert.Equal(t, int64(1), status.Statuses[0].ActiveQuarantineCount)
	assert.Equal(t, "quarantine", status.Statuses[0].ResetBlocker)

	skip := runKataOK(t, fx.bin, fx.spokeA.dirs,
		"federation", "quarantine", "skip", strconv.FormatInt(q.ID, 10),
		"--confirm", "SKIP FEDERATION BATCH "+strconv.FormatInt(q.ID, 10),
		"--reason", "accept skipped batch")
	assert.Contains(t, skip.stdout, "quarantine #"+strconv.FormatInt(q.ID, 10)+" skipped")

	status = runKataJSON[e2eFederationStatusBody](t, fx.bin, fx.spokeA.dirs,
		"federation", "status")
	require.Len(t, status.Statuses, 1)
	assert.Equal(t, int64(0), status.Statuses[0].ActiveQuarantineCount)
	assert.Empty(t, status.Statuses[0].ResetBlocker)

	hubDirs := hubClaimCLIDirs(t, fx)
	claim := runKataJSON[e2eClaimActionBody](t, fx.bin, hubDirs,
		"--project", fx.hubProject.Name, "federation", "lease", "acquire", fx.hubIssue.ShortID, "--as", "agent")
	require.True(t, claim.Granted)
	runKataOK(t, fx.bin, hubDirs,
		"--as", "agent", "--project", fx.hubProject.Name, "purge", fx.hubIssue.ShortID,
		"--force", "--confirm", "PURGE "+fx.hubProject.Name+"#"+fx.hubIssue.ShortID)

	waitForFederatedIssueGone(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)
	waitForFederatedIssueGone(t, fx.spokeB.db, fx.hubIssue.UID, fx.spokeB.stderr)
	survivorA := waitForFederatedIssue(t, fx.spokeA.db, survivor.UID, fx.spokeA.stderr)
	survivorB := waitForFederatedIssue(t, fx.spokeB.db, survivor.UID, fx.spokeB.stderr)
	assert.Equal(t, "survives purge reset", survivorA.Title)
	assert.Equal(t, "survives purge reset", survivorB.Title)

	show := runKataWantExit(t, fx.bin, fx.spokeA.dirs, 4,
		"--project", fx.hubProject.Name, "show", issueA.ShortID)
	assert.Contains(t, show.stderr, "issue not found")
}

type e2eFederationStatusBody struct {
	Statuses []struct {
		ActiveQuarantineCount int64  `json:"active_quarantine_count"`
		ResetBlocker          string `json:"reset_blocker,omitempty"`
	} `json:"statuses"`
}

func waitForFederatedIssueGone(t *testing.T, store *sqlitestore.Store, issueUID string, daemonStderr *safeBuffer) {
	t.Helper()
	var lastErr error
	require.Eventually(t, func() bool {
		_, err := store.IssueByUID(context.Background(), issueUID, db.IncludeDeletedYes)
		lastErr = err
		return federatedIssueGone(err)
	}, 5*time.Second, 50*time.Millisecond,
		"federated issue %s remained after reset; last lookup error: %v; daemon stderr: %s",
		issueUID, lastErr, daemonStderr.String())
}

func federatedIssueGone(err error) bool {
	return errors.Is(err, db.ErrNotFound)
}

func TestFederatedIssueGoneOnlyAcceptsNotFound(t *testing.T) {
	assert.True(t, federatedIssueGone(db.ErrNotFound))
	assert.False(t, federatedIssueGone(errors.New("scan failed")))
	assert.False(t, federatedIssueGone(nil))
}

func TestFederationStatusJSONDecodesInOperationalE2E(t *testing.T) {
	fx := setupFederationClaimE2E(t, "phase4-status-json")
	waitForFederatedIssue(t, fx.spokeA.db, fx.hubIssue.UID, fx.spokeA.stderr)

	res := runKataOK(t, fx.bin, fx.spokeA.dirs, "--json", "federation", "status")

	var body e2eFederationStatusBody
	require.NoError(t, json.Unmarshal([]byte(res.stdout), &body))
	require.Len(t, body.Statuses, 1)
}

//go:build !windows

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func TestSmoke_FederationPhase2BidirectionalSync(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spokeDirs := newE2EDirs(t)
	bin := buildKataBinary(t)
	spokeStderr := startDaemon(t, bin, append(spokeDirs.env(), "KATA_FEDERATION_PULL_INTERVAL_MS=60000"))
	spokeURL, spokeHTTP := connectDaemon(t, spokeDirs, spokeStderr)
	spokeDB, err := sqlitestore.Open(ctx, spokeDirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spokeDB.Close() })

	resp, err := spokeHTTP.Get(spokeURL + "/api/v1/instance") //nolint:gosec // test loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var inst struct {
		InstanceUID string `json:"instance_uid"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&inst))
	require.Equal(t, spokeDB.InstanceUID(), inst.InstanceUID)

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "hub baseline",
		Body:      "from hub",
		Author:    "agent",
	})
	require.NoError(t, err)
	var meta api.ProjectFederationBody
	decodePOST(t, hub.HTTP, hub.URL+"/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "phase2-bidirectional-token",
		SpokeInstanceUID: inst.InstanceUID,
		ProjectID:        nil,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	decodePOST(t, spokeHTTP, spokeURL+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"token":                   created.Token,
		"capabilities":            "pull,push",
		"push_enabled":            true,
	}, &replica)
	require.True(t, replica.Binding.PushEnabled)

	got := waitForFederatedIssue(t, spokeDB, hubIssue.UID, spokeStderr)
	require.Equal(t, "hub baseline", got.Title)
	assertFoldedProjectionMatch(t, hub.DB, spokeDB, hubProject.ID, replica.Project.ID, meta.ReplayHorizonEventID-1)

	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: hubIssue.ID,
		Author:  "agent",
		Body:    "hub follow-up before spoke wake",
	})
	require.NoError(t, err)

	resp = postJSON(t, spokeHTTP, spokeURL+"/api/v1/projects/"+strconv.FormatInt(replica.Project.ID, 10)+"/issues",
		map[string]any{"title": "spoke pushed", "body": "from spoke", "actor": "agent"})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	pushed := waitForFederatedTitle(t, hub.DB, "spoke pushed", spokeStderr, 2*time.Second)
	assert.Equal(t, "from spoke", pushed.Body)
	waitForFoldedProjectionMatch(t, hub.DB, spokeDB, hubProject.ID, replica.Project.ID, meta.ReplayHorizonEventID-1, spokeStderr)
}

func TestFederationPhase2PushWakeLatency(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spokeDirs := newE2EDirs(t)
	bin := buildKataBinary(t)
	spokeStderr := startDaemon(t, bin, append(spokeDirs.env(), "KATA_FEDERATION_PULL_INTERVAL_MS=60000"))
	spokeURL, spokeHTTP := connectDaemon(t, spokeDirs, spokeStderr)
	spokeDB, err := sqlitestore.Open(ctx, spokeDirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spokeDB.Close() })

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	var meta api.ProjectFederationBody
	decodePOST(t, hub.HTTP, hub.URL+"/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "phase2-push-token",
		SpokeInstanceUID: spokeDB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)

	var replica api.CreateFederationReplicaBody
	decodePOST(t, spokeHTTP, spokeURL+"/api/v1/federation/replicas", map[string]any{
		"hub_url":                 hub.URL,
		"hub_project_id":          hubProject.ID,
		"hub_project_uid":         meta.ProjectUID,
		"project_name":            meta.ProjectName,
		"replay_horizon_event_id": meta.ReplayHorizonEventID,
		"token":                   created.Token,
	}, &replica)
	_, err = spokeDB.EnableFederationPush(ctx, replica.Project.ID, 0)
	require.NoError(t, err)

	resp := postJSON(t, spokeHTTP, spokeURL+"/api/v1/projects/"+strconv.FormatInt(replica.Project.ID, 10)+"/issues",
		map[string]any{"title": "pushed quickly", "actor": "agent"})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := waitForFederatedTitle(t, hub.DB, "pushed quickly", spokeStderr, 2*time.Second)
	assert.Equal(t, "pushed quickly", got.Title)
}

func waitForFederatedTitle(
	t *testing.T,
	store *sqlitestore.Store,
	title string,
	daemonStderr *safeBuffer,
	timeout time.Duration,
) db.Issue {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var uid string
		err := store.QueryRowContext(context.Background(),
			`SELECT uid FROM issues WHERE title = ?`, title).Scan(&uid)
		if err == nil {
			issue, err := store.IssueByUID(context.Background(), uid, db.IncludeDeletedNo)
			require.NoError(t, err)
			return issue
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("federated issue title %q was not materialized: %v\ndaemon stderr: %s",
		title, lastErr, daemonStderr.String())
	return db.Issue{}
}

func waitForFoldedProjectionMatch(
	t *testing.T,
	hub *sqlitestore.Store,
	spoke *sqlitestore.Store,
	hubProjectID int64,
	spokeProjectID int64,
	hubAfterID int64,
	daemonStderr *safeBuffer,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastHub, lastSpoke db.FoldProjection
	for time.Now().Before(deadline) {
		var err error
		lastHub, lastSpoke, err = foldedProjections(t, hub, spoke, hubProjectID, spokeProjectID, hubAfterID)
		require.NoError(t, err)
		if assert.ObjectsAreEqual(lastHub.Issues, lastSpoke.Issues) &&
			assert.ObjectsAreEqual(lastHub.Comments, lastSpoke.Comments) &&
			assert.ObjectsAreEqual(lastHub.Labels, lastSpoke.Labels) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	assert.Equal(t, lastHub.Issues, lastSpoke.Issues)
	assert.Equal(t, lastHub.Comments, lastSpoke.Comments)
	assert.Equal(t, lastHub.Labels, lastSpoke.Labels)
	t.Fatalf("folded projections did not converge\ndaemon stderr: %s", daemonStderr.String())
}

func foldedProjections(
	t *testing.T,
	hub *sqlitestore.Store,
	spoke *sqlitestore.Store,
	hubProjectID int64,
	spokeProjectID int64,
	hubAfterID int64,
) (db.FoldProjection, db.FoldProjection, error) {
	t.Helper()
	ctx := context.Background()
	hubEvents, err := hub.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: hubProjectID,
		AfterID:   hubAfterID,
		Limit:     1000,
	})
	if err != nil {
		return db.FoldProjection{}, db.FoldProjection{}, err
	}
	spokeEvents, err := spoke.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: spokeProjectID,
		Limit:     1000,
	})
	if err != nil {
		return db.FoldProjection{}, db.FoldProjection{}, err
	}
	return db.FoldEvents(foldEvents(hubEvents)), db.FoldEvents(foldEvents(spokeEvents)), nil
}

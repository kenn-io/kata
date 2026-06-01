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

func TestSmoke_FederationPhase1PullReplication(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	spokeDirs := newE2EDirs(t)
	bin := buildKataBinary(t)
	spokeStderr := startDaemon(t, bin, append(spokeDirs.env(), "KATA_FEDERATION_PULL_INTERVAL_MS=25"))
	spokeURL, spokeHTTP := connectDaemon(t, spokeDirs, spokeStderr)
	spokeDB, err := sqlitestore.Open(ctx, spokeDirs.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spokeDB.Close() })

	hubProject, err := hub.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	hubIssue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "replicated work",
		Body:      "from the hub",
		Author:    "agent",
		Labels:    []string{"area:db"},
	})
	require.NoError(t, err)
	_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{
		IssueID: hubIssue.ID,
		Author:  "agent",
		Body:    "baseline comment",
	})
	require.NoError(t, err)

	var meta api.ProjectFederationBody
	decodePOST(t, hub.HTTP, hub.URL+"/api/v1/projects/"+strconv.FormatInt(hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "agent"}, &meta)
	created, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "phase1-pull-token",
		SpokeInstanceUID: spokeDB.InstanceUID(),
		ProjectID:        &hubProject.ID,
		Capabilities:     "pull",
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

	got := waitForFederatedIssue(t, spokeDB, hubIssue.UID, spokeStderr)
	assert.Equal(t, "replicated work", got.Title)
	assert.Equal(t, "from the hub", got.Body)
	assertFoldedProjectionMatch(t, hub.DB, spokeDB, hubProject.ID, replica.Project.ID, meta.ReplayHorizonEventID-1)
}

func decodePOST(t *testing.T, client *http.Client, url string, body, out any) {
	t.Helper()
	resp := postJSON(t, client, url, body)
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, http.StatusOK, resp.StatusCode, "POST %s", url)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func waitForFederatedIssue(t *testing.T, store *sqlitestore.Store, issueUID string, daemonStderr *safeBuffer) db.Issue {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		issue, err := store.IssueByUID(context.Background(), issueUID, db.IncludeDeletedYes)
		if err == nil {
			return issue
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("federated issue %s was not materialized: %v\ndaemon stderr: %s",
		issueUID, lastErr, daemonStderr.String())
	return db.Issue{}
}

func assertFoldedProjectionMatch(t *testing.T, hub, spoke *sqlitestore.Store, hubProjectID, spokeProjectID, hubAfterID int64) {
	t.Helper()
	ctx := context.Background()
	hubEvents, err := hub.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: hubProjectID,
		AfterID:   hubAfterID,
		Limit:     1000,
	})
	require.NoError(t, err)
	spokeEvents, err := spoke.EventsAfter(ctx, db.EventsAfterParams{
		ProjectID: spokeProjectID,
		Limit:     1000,
	})
	require.NoError(t, err)
	hubFold := db.FoldEvents(foldEvents(hubEvents))
	spokeFold := db.FoldEvents(foldEvents(spokeEvents))
	assert.Equal(t, hubFold.Issues, spokeFold.Issues)
	assert.Equal(t, hubFold.Comments, spokeFold.Comments)
	assert.Equal(t, hubFold.Labels, spokeFold.Labels)
}

func foldEvents(events []db.Event) []db.FoldEvent {
	out := make([]db.FoldEvent, 0, len(events))
	for _, ev := range events {
		var issueUID string
		if ev.IssueUID != nil {
			issueUID = *ev.IssueUID
		}
		var relatedUID string
		if ev.RelatedIssueUID != nil {
			relatedUID = *ev.RelatedIssueUID
		}
		out = append(out, db.FoldEvent{
			UID:               ev.UID,
			OriginInstanceUID: ev.OriginInstanceUID,
			ProjectUID:        ev.ProjectUID,
			IssueUID:          issueUID,
			RelatedIssueUID:   relatedUID,
			Type:              ev.Type,
			Actor:             ev.Actor,
			HLCPhysicalMS:     ev.HLCPhysicalMS,
			HLCCounter:        ev.HLCCounter,
			CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Payload:           json.RawMessage(ev.Payload),
		})
	}
	return out
}

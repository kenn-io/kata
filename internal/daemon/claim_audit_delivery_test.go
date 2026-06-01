package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestClaimCloseReleaseBroadcastsAndEnqueuesGeneratedClaimAudit(t *testing.T) {
	ctx := context.Background()
	hooksSink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:          d.db,
		StartedAt:   d.now,
		Broadcaster: bcast,
		Hooks:       hooksSink,
	})
	project, issue := createClaimHubIssueInDB(t, d.db)
	_, err := d.db.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: d.db.InstanceUID(),
			Holder:            "agent",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	resp, raw := doReq(t, ts, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "actions/close"),
		map[string]any{"actor": "agent", "source": "tui"}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	first := receiveMsg(t, sub.Ch, time.Second, "issue.closed broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "claim.released broadcast")
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, "issue.closed", first.Event.Type)
	assert.Equal(t, "claim.released", second.Event.Type)
	assert.Greater(t, second.Event.ID, first.Event.ID)

	captured := hooksSink.snapshot()
	require.Len(t, captured, 2)
	assert.Equal(t, "issue.closed", captured[0].Type)
	assert.Equal(t, "claim.released", captured[1].Type)
	assert.Greater(t, captured[1].ID, captured[0].ID)
}

func TestIngestClaimViolationBroadcastsAndEnqueuesGeneratedClaimAudit(t *testing.T) {
	ctx := context.Background()
	hooksSink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:          d.db,
		StartedAt:   d.now,
		Broadcaster: bcast,
		Hooks:       hooksSink,
	})
	project, issue := createClaimHubIssueInDB(t, d.db)
	enrollment := createClaimAuditDeliveryEnrollment(t, d.db, project.ID)
	_, err := d.db.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "claim-holder",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	ev := claimAuditDeliveryRemoteEvent(t, project, federationTestSpokeUID, issue.UID,
		"01HZNQ7VFPK1XGD8R5MABCD4ED", "issue.updated", "remote-agent", 101)
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	resp, raw := doReq(t, ts, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest",
		federationIngestBody(federationIngestEnvelope(t, 1, ev)), bearer(enrollment.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	first := receiveMsg(t, sub.Ch, time.Second, "ingested issue.updated broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "claim.violated broadcast")
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, ev.EventUID, first.Event.UID)
	assert.Equal(t, "issue.updated", first.Event.Type)
	assert.Equal(t, "claim.violated", second.Event.Type)
	assert.Greater(t, second.Event.ID, first.Event.ID)

	captured := hooksSink.snapshot()
	require.Len(t, captured, 2)
	assert.Equal(t, "issue.updated", captured[0].Type)
	assert.Equal(t, "claim.violated", captured[1].Type)
	assert.Greater(t, captured[1].ID, captured[0].ID)
}

func TestIngestClaimCloseReleaseBroadcastsAndEnqueuesGeneratedClaimAudit(t *testing.T) {
	ctx := context.Background()
	hooksSink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:          d.db,
		StartedAt:   d.now,
		Broadcaster: bcast,
		Hooks:       hooksSink,
	})
	project, issue := createClaimHubIssueInDB(t, d.db)
	enrollment := createClaimAuditDeliveryEnrollment(t, d.db, project.ID)
	_, err := d.db.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "remote-agent",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	ev := claimAuditDeliveryRemoteEvent(t, project, federationTestSpokeUID, issue.UID,
		"01HZNQ7VFPK1XGD8R5MABCD4EE", "issue.closed", "remote-agent", 102)
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	resp, raw := doReq(t, ts, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest",
		federationIngestBody(federationIngestEnvelope(t, 1, ev)), bearer(enrollment.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	first := receiveMsg(t, sub.Ch, time.Second, "ingested issue.closed broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "claim.released broadcast")
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, ev.EventUID, first.Event.UID)
	assert.Equal(t, "issue.closed", first.Event.Type)
	assert.Equal(t, "claim.released", second.Event.Type)
	assert.Greater(t, second.Event.ID, first.Event.ID)

	captured := hooksSink.snapshot()
	require.Len(t, captured, 2)
	assert.Equal(t, "issue.closed", captured[0].Type)
	assert.Equal(t, "claim.released", captured[1].Type)
	assert.Greater(t, captured[1].ID, captured[0].ID)
}

func createClaimAuditDeliveryEnrollment(t *testing.T, store *sqlitestore.Store, projectID int64) db.CreatedFederationEnrollment {
	t.Helper()
	created, err := store.CreateFederationEnrollment(context.Background(), db.CreateFederationEnrollmentParams{
		Token:            "claim-audit-delivery-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &projectID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	return created
}

func claimAuditDeliveryRemoteEvent(
	t *testing.T,
	project db.Project,
	spokeUID string,
	issueUID string,
	eventUID string,
	eventType string,
	actor string,
	hlcPhysicalMS int64,
) db.RemoteEvent {
	t.Helper()
	payload := json.RawMessage(`{"issue_uid":"` + issueUID + `","title":"remote title"}`)
	if eventType == "issue.closed" {
		payload = json.RawMessage(`{"issue_uid":"` + issueUID + `","reason":"done","closed_at":"2026-05-23T12:00:00.000Z"}`)
	}
	ev := db.RemoteEvent{
		EventUID:          eventUID,
		OriginInstanceUID: spokeUID,
		ProjectUID:        project.UID,
		ProjectName:       project.Name,
		IssueUID:          &issueUID,
		Type:              eventType,
		Actor:             actor,
		HLCPhysicalMS:     hlcPhysicalMS,
		HLCCounter:        0,
		Payload:           payload,
		CreatedAt:         time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:           ev.Payload,
	})
	require.NoError(t, err)
	ev.ContentHash = hash
	return ev
}

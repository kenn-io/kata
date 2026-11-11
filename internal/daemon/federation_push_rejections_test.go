package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// A stale comment must not block independent work in the enrolled project.
func TestFederationPushMissingCurrentProjectEnrollmentDoesNotBlockBatch(t *testing.T) {
	env := testenv.New(t)
	source, target, moved := seedMovePair(t, env)
	for _, project := range []db.Project{source, target} {
		_, err := env.DB.EnableProjectFederation(t.Context(), project.ID, "tester")
		require.NoError(t, err)
	}
	independent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Independent work", Author: "tester"})
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: moved.Revision, Actor: "tester"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester", AllowAdoptionSnapshotAuthors: true})
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	makeEnvelope := func(id int64, issue db.Issue, kind string, payload any) api.FederationIngestEventEnvelope {
		t.Helper()
		eventUID, err := uid.New()
		require.NoError(t, err)
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		remote := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &issue.UID, Type: kind, Actor: "tester", Payload: jsontext.Value(encoded), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
		remote.ContentHash, err = db.RemoteEventContentHash(remote)
		require.NoError(t, err)
		return api.FederationIngestEventEnvelope{EventID: id, EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: source.UID, ProjectName: source.Name, IssueUID: &issue.UID, Type: kind, Actor: remote.Actor, Payload: remote.Payload, HLCPhysicalMS: remote.HLCPhysicalMS, ContentHash: remote.ContentHash, CreatedAt: at}
	}
	stale := makeEnvelope(1, moved, "issue.commented", map[string]string{"comment_uid": commentUID, "author": "tester", "body": "Stale source comment", "created_at": at.Format(time.RFC3339Nano)})
	valid := makeEnvelope(2, independent, "issue.updated", map[string]string{"uid": independent.UID, "title": "Independent update arrived"})
	body := api.FederationIngestEventsRequestBody{SchemaVersion: db.CurrentSchemaVersion(), Events: []api.FederationIngestEventEnvelope{stale, valid}}
	headers := bearer(grant.Token)
	headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
	headers[db.FederationEventRejectionsHeader] = "1"
	response, raw := envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	require.Equalf(t, http.StatusOK, response.StatusCode, "reject only the stale comment: %s", raw)
	var result api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.RejectedEvents, 1)
	require.Equal(t, stale.EventUID, result.RejectedEvents[0].EventUID)
	require.Contains(t, result.RejectedEvents[0].Message, "matching project enrollment or push proof is missing")
	body.MoveSourceTokens = map[string]string{target.UID: "invalid-supplemental-proof"}
	response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	require.Equalf(t, http.StatusOK, response.StatusCode, "%s", raw)
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Len(t, result.RejectedEvents, 1)
	require.Equal(t, stale.EventUID, result.RejectedEvents[0].EventUID)
}

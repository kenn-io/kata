package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	grant, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &source.ID, Capabilities: "push,pull", Actor: "tester"})
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
	makeEnvelope := func(id int64, issue db.Issue, kind string, payload map[string]string) api.FederationIngestEventEnvelope {
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
	response, raw := envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	assert.Equal(t, http.StatusForbidden, response.StatusCode, "older clients retain atomic failure")
	assert.Contains(t, string(raw), "federation_project_not_enrolled")
	unchanged, err := env.DB.IssueByID(t.Context(), independent.ID)
	require.NoError(t, err)
	assert.Equal(t, independent.Title, unchanged.Title)
	headers["X-Kata-Federation-Event-Rejections"] = "1"
	response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	t.Logf("mixed-batch response: status=%d body=%s", response.StatusCode, raw)
	assert.Equalf(t, http.StatusOK, response.StatusCode, "reject only the stale comment; accept independent source work: %s", raw)
	if response.StatusCode == http.StatusOK {
		var result api.FederationIngestEventsBody
		require.NoError(t, json.Unmarshal(raw, &result))
		assert.Equal(t, 1, result.Accepted)
		require.Len(t, result.RejectedEvents, 1)
		assert.Equal(t, stale.EventUID, result.RejectedEvents[0].EventUID)
		assert.Equal(t, moved.UID, result.RejectedEvents[0].IssueUID)
		assert.Equal(t, target.UID, result.RejectedEvents[0].CurrentProjectUID)
		assert.Equal(t, target.Name, result.RejectedEvents[0].CurrentProjectName)
		assert.Equal(t, "federation_project_not_enrolled", result.RejectedEvents[0].Code)
	}
	got, err := env.DB.IssueByID(t.Context(), independent.ID)
	require.NoError(t, err)
	assert.Equal(t, "Independent update arrived", got.Title, "independent event must arrive despite the stale comment")
	comments, err := env.DB.CommentsByIssue(t.Context(), moved.ID)
	require.NoError(t, err)
	assert.Empty(t, comments, "missing target enrollment must never grant authority to write the moved issue")
	body.Events = []api.FederationIngestEventEnvelope{stale}
	response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	require.Equal(t, http.StatusOK, response.StatusCode)
	var rejectedOnly api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &rejectedOnly))
	require.Zero(t, rejectedOnly.Accepted)
	require.Len(t, rejectedOnly.RejectedEvents, 1)
	body.MoveSourceTokens = map[string]string{target.UID: "invalid-supplemental-proof"}
	response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Contains(t, string(raw), "auth_invalid", "supplied invalid credentials remain request-wide failures")
	body.MoveSourceTokens = nil
	require.NoError(t, env.DB.RevokeFederationEnrollment(t.Context(), grant.Enrollment.ID))
	response, raw = envDoRaw(t, env, http.MethodPost, projectPath(source.ID)+"/federation/events:ingest", body, headers)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Contains(t, string(raw), "auth_invalid", "revoked route credentials never become partial success")

}

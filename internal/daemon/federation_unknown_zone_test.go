package daemon_test

import (
	"encoding/json/jsontext"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationUnknownZoneSender(t *testing.T) {
	const zone = "Example/SenderOnly"
	_, err := time.LoadLocation(zone)
	require.Error(t, err)

	env := testenv.New(t)
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: federationTestSpokeUID, ProjectID: &project.ID, Capabilities: "push", Actor: "tester",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueSnapshotEvent(t, project, federationTestSpokeUID, "tester")
	ev.Type = "issue.created"
	ev.Payload = jsontext.Value(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EE","short_id":"cd4ee","title":"remote","body":"","author":"tester","status":"open","metadata":{"scheduled_on":"2026-01-15T09:00[Example/SenderOnly]"},"created_at":"2026-05-23T12:00:00.000Z"}`)
	ev.ContentHash, err = db.RemoteEventContentHash(ev)
	require.NoError(t, err)
	headers := bearer(created.Token)
	headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
	resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest", federationIngestBody(federationIngestEnvelope(t, 1, ev)), headers)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "Example/SenderOnly")
}

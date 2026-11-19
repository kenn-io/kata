package daemon_test

import (
	"encoding/json/jsontext"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationUnknownZoneSender(t *testing.T) {
	const zone = "Example/SenderOnly"
	if os.Getenv("KATA_TEST_ZONE_SENDER") == "1" {
		require.NoError(t, metadata.Validate(metadata.IssueRegistry, "scheduled_on", jsontext.Value(`"2026-01-15T09:00[Example/SenderOnly]"`)))
		return
	}
	_, err := time.LoadLocation(zone)
	require.Error(t, err)
	// TZif v1 with one UTC zone and no transitions.
	contents := []byte("TZif\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" +
		"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00" +
		"\x00\x00\x00\x01\x00\x00\x00\x04\x00\x00\x00\x00\x00\x00UTC\x00")
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "Example"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, zone), contents, 0o600))
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFederationUnknownZoneSender$") //nolint:gosec // G204: rerun this test binary with the sender's timezone data.
	cmd.Env = append(os.Environ(), "KATA_TEST_ZONE_SENDER=1", "ZONEINFO="+dir)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))

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
	_, err = env.DB.ReadyIssues(t.Context(), project.ID, 100, db.ReadyIssuesFilter{})
	require.NoError(t, err)
}

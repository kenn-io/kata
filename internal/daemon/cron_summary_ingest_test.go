package daemon_test

import (
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronSummaryIngestValidation(t *testing.T) {
	for _, summary := range []string{
		`{"version":1,"reason":"INVALID"}`,
		`{"version":1,"input_tokens":-1}`,
		`{"version":1,"reason":null}`,
		`{"version":1,"trigger":null}`,
		`{"version":1,"usage":null}`,
		`{"version":1,"steps":null}`,
		`{"version":1,"log_ref":null}`,
	} {
		t.Run(summary, func(t *testing.T) {
			env := testenv.New(t)
			ctx := t.Context()
			project := createFederatedHubProject(t, env, "hub-project")
			created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
				Token: "summary-ingest-fixture", SpokeInstanceUID: federationTestSpokeUID,
				ProjectID: &project.ID, Capabilities: "push", Actor: "tester",
			})
			require.NoError(t, err)
			job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Actor: "tester", Name: "Review", Definition: cronHTTPDefinition(t)})
			require.NoError(t, err)
			event := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
			event.Type = "cron.run.observed"
			event.IssueUID = nil
			event.Payload = jsontext.Value(fmt.Sprintf(`{"project_uid":%q,"uid":%q,"job_uid":%q,"definition_event_uid":%q,"actor":"tester","status":"failed","revision":1,"created_at":"2026-05-23T12:00:00Z","updated_at":"2026-05-23T12:00:00Z","summary":%s}`, project.UID, cronHTTPUID(t), job.UID, job.DefinitionEventUID, summary))
			event.ContentHash, err = db.EventContentHash(db.EventHashInput{
				UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
				Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
				CreatedAt: event.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"), Payload: event.Payload,
			})
			require.NoError(t, err)
			body := federationIngestBody(federationIngestEnvelope(t, 17, event))
			before, err := env.DB.MaxEventID(ctx)
			require.NoError(t, err)
			headers := bearer(created.Token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest", body,
				headers)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), `"code":"validation"`)
			require.Contains(t, string(raw), "invalid cron document", "must reach summary validation rather than envelope rejection")
			after, err := env.DB.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			runs, err := env.DB.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Empty(t, runs)
		})
	}
}

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
			event := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
			event.Type = "cron.run.observed"
			event.Payload = jsontext.Value(fmt.Sprintf(`{"summary":%s}`, summary))
			body := federationIngestBody(federationIngestEnvelope(t, 17, event))
			before, err := env.DB.MaxEventID(ctx)
			require.NoError(t, err)
			headers := bearer(created.Token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(project.ID)+"/federation/events:ingest", body,
				headers)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
			require.Contains(t, string(raw), `"code":"validation"`)
			after, err := env.DB.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, before, after)
			runs, err := env.DB.ListCronRuns(ctx, db.CronRunList{ProjectID: project.ID})
			require.NoError(t, err)
			require.Empty(t, runs)
		})
	}
}

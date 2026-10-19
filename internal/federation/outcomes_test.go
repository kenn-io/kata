package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type outcomeRecorder struct {
	daemon.TelemetryReporter
	events []map[string]any
}

func (r *outcomeRecorder) Capture(event string, properties map[string]any) error {
	if event == "sync_failed" {
		r.events = append(r.events, properties)
	}
	return nil
}

func TestOutcomesFailingSpoke(t *testing.T) {
	recorder := &outcomeRecorder{}
	observe := daemon.NewSyncOutcomeObserver(recorder)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(hub.Close)
	spoke := testenv.New(t)
	t.Setenv("KATA_HOME", t.TempDir())
	project, err := spoke.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: 1, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: 1, Token: "example-token"}))
	runner := &Runner{DB: spoke.DB, OnSpokeResult: func(id int64, err error) { observe("federation", id, err) }}
	require.Error(t, runner.RunOnce(t.Context()))
	require.Len(t, recorder.events, 1)
	require.Equal(t, map[string]any{"sync_source": "federation", "reason": "auth"}, recorder.events[0])
}

package federation

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestOutcomesFailingSpoke(t *testing.T) {
	var results []error
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
	runner := &Runner{DB: spoke.DB, OnSpokeResult: func(id int64, err error) {
		require.Equal(t, project.ID, id)
		results = append(results, err)
	}}
	require.Error(t, runner.RunOnce(t.Context()))
	require.Len(t, results, 1)
	status, ok := errors.AsType[interface {
		error
		HTTPStatus() int
	}](results[0])
	require.True(t, ok, "spoke error must retain its HTTP status: %v", results[0])
	require.Equal(t, http.StatusUnauthorized, status.HTTPStatus())
}

package daemon_test

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

func TestCronExecutionPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("requires isolated PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "company-project")
	require.NoError(t, err)
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "company-member", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	executionBinding(t, store, project, db.FederationRoleSpoke, true, false)
	srv := daemon.NewServer(daemon.ServerConfig{DB: store})
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil))
	require.Equalf(t, 200, recorder.Code, "%s", recorder.Body.String())
	var result struct {
		Execution api.CronExecutionPolicy `json:"execution"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	require.Equal(t, "none", result.Execution.Mode)
	require.Equal(t, "company-member", result.Execution.MemberActor)
	recorder = httptest.NewRecorder()
	request := httptest.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, cronHTTPUID(t)), strings.NewReader(fmt.Sprintf(`{"actor":"observer","job_uid":%q,"definition_event_uid":%q,"status":"running","summary":{"version":1},"expected_revision":0}`, job.UID, job.DefinitionEventUID)))
	request.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(recorder, request)
	require.Equalf(t, 403, recorder.Code, "%s", recorder.Body.String())
	require.Contains(t, recorder.Body.String(), "cron_execution_not_enabled")
}

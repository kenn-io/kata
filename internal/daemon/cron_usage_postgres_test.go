package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/agentsview"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

// Usage reads must not change persisted observations or the event cursor on
// PostgreSQL, matching the SQLite contract.
func TestCronUsagePostgresDoesNotWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	def := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Example"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Example"}, Overlap: "allow", Catchup: "skip"}
	job, _, err := store.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "Example", Actor: "worker", Definition: def})
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "example", SessionID: "session"}}
	id := cronHTTPUID(t)
	_, err = store.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: id, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}})
	require.NoError(t, err)
	before, err := store.CronRun(ctx, project.ID, id)
	require.NoError(t, err)
	cursor, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(upstream.Close)
	service, err := agentsview.NewService(config.ResolvedAgentsView{URL: upstream.URL, BrowserURL: upstream.URL}, false)
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, AgentsView: service})
	t.Cleanup(func() { _ = server.Close() })
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/usage", project.ID, id), nil)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	after, err := store.CronRun(ctx, project.ID, id)
	require.NoError(t, err)
	require.Equal(t, before, after)
	next, err := store.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, next)
}

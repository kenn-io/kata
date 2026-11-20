package daemon_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
)

type postgresHubProcess struct {
	store  *pgstore.Store
	reads  *federationReadProbe
	server *httptest.Server
}

func newPostgresHubProcess(t testing.TB, dsn string) postgresHubProcess {
	t.Helper()
	store, err := pgstore.Open(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	reads := newFederationReadProbe(store)
	hub := daemon.NewServer(daemon.ServerConfig{DB: reads.storage(), StartedAt: time.Now().UTC()})
	t.Cleanup(func() { _ = hub.Close() })
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)
	return postgresHubProcess{store: store, reads: reads, server: server}
}

func TestFederationPollWaitWakesAcrossPostgresHubProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	ctx := t.Context()
	dsn, cleanup := pgstoretest.NewDatabase(t, ctx)
	t.Cleanup(cleanup)
	writer := newPostgresHubProcess(t, dsn)
	poller := newPostgresHubProcess(t, dsn)
	project, err := writer.store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, err = writer.store.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	_, err = writer.store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "long-poll-token", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(t, err)
	cursor := pollPostgresHub(t, poller.server.URL, project.ID, 0, "").body.NextAfterID

	reads := poller.reads.readsReturned()
	done := make(chan longPollResult, 1)
	go func() { done <- pollPostgresHub(t, poller.server.URL, project.ID, cursor, "30") }()
	// The poll subscribes to wakeups before its first read, so the commit
	// below reaches it only through the wake bridge.
	poller.reads.waitForReads(t, reads+1)
	createIssueOnPostgresHub(t, writer.server.URL, project.ID, "Committed by another hub process")
	select {
	case result := <-done:
		require.Equal(t, http.StatusOK, result.status, "%s", result.raw)
		require.NotEmpty(t, result.body.Events)
		require.Less(t, result.elapsed, 10*time.Second)
	case <-time.After(15 * time.Second):
		t.Fatal("held poll on one hub process did not wake for an event committed by another")
	}
}

func pollPostgresHub(t testing.TB, baseURL string, projectID, afterID int64, wait string) longPollResult {
	t.Helper()
	url := fmt.Sprintf("%s%s/federation/events?after_id=%d", baseURL, projectPath(projectID), afterID)
	if wait != "" {
		url += "&wait=" + wait
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer long-poll-token")
	started := time.Now()
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback daemon
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	return decodeLongPollResponse(t, response, time.Since(started))
}

func createIssueOnPostgresHub(t testing.TB, baseURL string, projectID int64, title string) {
	t.Helper()
	body := fmt.Sprintf(`{"actor":"tester","title":%q}`, title)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		baseURL+projectPath(projectID)+"/issues", bytes.NewBufferString(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback daemon
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
}

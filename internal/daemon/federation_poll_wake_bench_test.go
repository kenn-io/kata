package daemon_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// benchHubIntervalFallback is the spoke runner's default interval. A poll the
// hub answers at once, empty, is retried after it, the way a spoke would.
const benchHubIntervalFallback = 30 * time.Second

type benchHub struct {
	writerURL string
	pollerURL string
	projectID int64
}

func newBenchHubServer(b *testing.B, store db.Storage) string {
	b.Helper()
	hub := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	b.Cleanup(func() { _ = hub.Close() })
	server := httptest.NewServer(hub.Handler())
	b.Cleanup(server.Close)
	return server.URL
}

func newBenchHub(b *testing.B, writer, poller db.Storage) benchHub {
	b.Helper()
	ctx := b.Context()
	project, err := writer.CreateProject(ctx, "hub-project")
	require.NoError(b, err)
	_, err = writer.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(b, err)
	_, err = writer.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "bench-pull-token", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(b, err)
	hub := benchHub{writerURL: newBenchHubServer(b, writer), projectID: project.ID}
	hub.pollerURL = hub.writerURL
	if poller != writer {
		hub.pollerURL = newBenchHubServer(b, poller)
	}
	return hub
}

type benchPollPage struct {
	Events      []json.RawMessage `json:"events"`
	NextAfterID int64             `json:"next_after_id"`
}

func (h benchHub) poll(b *testing.B, afterID int64, wait string) benchPollPage {
	url := fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=%d", h.pollerURL, h.projectID, afterID)
	if wait != "" {
		url += "&wait=" + wait
	}
	request, err := http.NewRequestWithContext(b.Context(), http.MethodGet, url, nil)
	require.NoError(b, err)
	request.Header.Set("Authorization", "Bearer bench-pull-token")
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	require.NoError(b, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(b, http.StatusOK, response.StatusCode)
	var page benchPollPage
	require.NoError(b, json.NewDecoder(response.Body).Decode(&page))
	return page
}

func (h benchHub) commit(b *testing.B) {
	request, err := http.NewRequestWithContext(b.Context(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues", h.writerURL, h.projectID),
		bytes.NewBufferString(`{"actor":"tester","title":"Wakes the held poll"}`))
	require.NoError(b, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	require.NoError(b, err)
	_ = response.Body.Close()
	require.Equal(b, http.StatusOK, response.StatusCode)
}

// run measures the time from a commit to a caught-up poll returning it. The
// poll is parked before the commit, so a hub that holds polls answers on the
// wake; one that does not is polled again after the spoke interval.
func (h benchHub) run(b *testing.B) {
	cursor := h.poll(b, 0, "").NextAfterID
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		delivered := make(chan int64, 1)
		go func() {
			for {
				page := h.poll(b, cursor, "45")
				if len(page.Events) > 0 {
					delivered <- page.NextAfterID
					return
				}
				//nolint:kennlint // sleeptest: models the spoke interval a hub without held polls leaves to the spoke.
				time.Sleep(benchHubIntervalFallback)
			}
		}()
		//nolint:kennlint // sleeptest: lets the poll park in the hub over real HTTP before the commit.
		time.Sleep(100 * time.Millisecond)
		b.StartTimer()
		h.commit(b)
		cursor = <-delivered
	}
}

// BenchmarkFederationPollWake measures hub commit to poll delivery. Run it
// with -benchtime=1x: a hub without held polls takes a whole interval.
func BenchmarkFederationPollWake(b *testing.B) {
	b.Run("sqlite", func(b *testing.B) {
		store, err := sqlitestore.Open(b.Context(), filepath.Join(b.TempDir(), "hub.db"))
		require.NoError(b, err)
		b.Cleanup(func() { _ = store.Close() })
		newBenchHub(b, store, store).run(b)
	})
	openPostgres := func(b *testing.B, dsn string) *pgstore.Store {
		store, err := pgstore.Open(b.Context(), dsn)
		require.NoError(b, err)
		b.Cleanup(func() { _ = store.Close() })
		return store
	}
	b.Run("postgres", func(b *testing.B) {
		dsn := benchPostgresDatabase(b)
		store := openPostgres(b, dsn)
		newBenchHub(b, store, store).run(b)
	})
	b.Run("postgres-two-processes", func(b *testing.B) {
		dsn := benchPostgresDatabase(b)
		newBenchHub(b, openPostgres(b, dsn), openPostgres(b, dsn)).run(b)
	})
}

// benchPostgresDatabase creates an empty database on the server named by
// KATA_TEST_POSTGRES_DSN and drops it when the benchmark ends.
func benchPostgresDatabase(b *testing.B) string {
	b.Helper()
	serverDSN := os.Getenv("KATA_TEST_POSTGRES_DSN")
	if serverDSN == "" {
		b.Skip("KATA_TEST_POSTGRES_DSN is not set")
	}
	server, err := sql.Open("pgx", serverDSN)
	require.NoError(b, err)
	b.Cleanup(func() { _ = server.Close() })
	name := "kata_bench_" + strings.ToLower(strings.ReplaceAll(b.Name(), "/", "_")) + fmt.Sprintf("_%d", time.Now().UnixNano())
	_, err = server.ExecContext(b.Context(), `CREATE DATABASE "`+name+`"`) // #nosec G202 -- generated benchmark database name.
	require.NoError(b, err)
	b.Cleanup(func() {
		_, _ = server.ExecContext(context.WithoutCancel(b.Context()), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`) // #nosec G202 -- generated benchmark database name.
	})
	parsed, err := url.Parse(serverDSN)
	require.NoError(b, err)
	parsed.Path = "/" + name
	return parsed.String()
}

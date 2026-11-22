package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/pgstore/pgstoretest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// benchHubIntervalFallback is the spoke runner's default interval. A poll the
// hub answers at once, empty, is retried after it, the way a spoke would.
const benchHubIntervalFallback = 30 * time.Second

type benchHub struct {
	writerURL string
	pollerURL string
	projectID int64
	token     string
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
	hub := benchHub{writerURL: newBenchHubServer(b, writer), projectID: project.ID, token: "bench-pull-token"}
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

// pollPage reports failures as an error, so a worker goroutine can hand them
// back to the benchmark goroutine.
func (h benchHub) pollPage(ctx context.Context, afterID int64, wait string) (benchPollPage, error) {
	url := fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=%d", h.pollerURL, h.projectID, afterID)
	if wait != "" {
		url += "&wait=" + wait
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return benchPollPage{}, err
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	if err != nil {
		return benchPollPage{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return benchPollPage{}, fmt.Errorf("poll answered %d", response.StatusCode)
	}
	var page benchPollPage
	err = json.NewDecoder(response.Body).Decode(&page)
	return page, err
}

func (h benchHub) commit(b *testing.B) {
	request, err := http.NewRequestWithContext(b.Context(), http.MethodPost,
		fmt.Sprintf("%s/api/v1/projects/%d/issues", h.writerURL, h.projectID),
		// A unique title keeps the hub's duplicate-issue check from refusing a repeat.
		bytes.NewBufferString(fmt.Sprintf(`{"actor":"tester","title":"Wakes the held poll %d"}`, time.Now().UnixNano())))
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
	page, err := h.pollPage(b.Context(), 0, "")
	require.NoError(b, err)
	cursor := page.NextAfterID
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		type delivery struct {
			cursor int64
			err    error
		}
		delivered := make(chan delivery, 1)
		go func() {
			for {
				page, err := h.pollPage(b.Context(), cursor, "45")
				if err != nil || len(page.Events) > 0 {
					delivered <- delivery{cursor: page.NextAfterID, err: err}
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
		select {
		case got := <-delivered:
			require.NoError(b, got.err)
			cursor = got.cursor
		case <-time.After(2 * benchHubIntervalFallback):
			b.Fatal("the committed event never reached the poll")
		}
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
	newPostgres := func(b *testing.B) string {
		// A benchmark never starts a container; it measures a server the
		// caller names.
		if os.Getenv("KATA_TEST_POSTGRES_DSN") == "" {
			b.Skip("set KATA_TEST_POSTGRES_DSN to benchmark PostgreSQL wakes")
		}
		dsn, cleanup := pgstoretest.NewDatabase(b, b.Context())
		b.Cleanup(cleanup)
		return dsn
	}
	openPostgres := func(b *testing.B, dsn string) *pgstore.Store {
		store, err := pgstore.Open(b.Context(), dsn)
		require.NoError(b, err)
		b.Cleanup(func() { _ = store.Close() })
		return store
	}
	b.Run("postgres", func(b *testing.B) {
		store := openPostgres(b, newPostgres(b))
		newBenchHub(b, store, store).run(b)
	})
	b.Run("postgres-two-processes", func(b *testing.B) {
		dsn := newPostgres(b)
		newBenchHub(b, openPostgres(b, dsn), openPostgres(b, dsn)).run(b)
	})
}

// BenchmarkFederationPollWakeFanout measures the time and unrelated reads
// after a commit wakes held polls on a second PostgreSQL hub server.
func BenchmarkFederationPollWakeFanout(b *testing.B) {
	if os.Getenv("KATA_TEST_POSTGRES_DSN") == "" {
		b.Skip("set KATA_TEST_POSTGRES_DSN to benchmark PostgreSQL wakes")
	}
	for _, count := range []int{8, 32} {
		b.Run(fmt.Sprintf("projects-%d", count), func(b *testing.B) {
			dsn, cleanup := pgstoretest.NewDatabase(b, b.Context())
			b.Cleanup(cleanup)
			setup, err := pgstore.Open(b.Context(), dsn)
			require.NoError(b, err)
			hubs := make([]benchHub, count)
			for i := range hubs {
				project, err := setup.CreateProject(b.Context(), fmt.Sprintf("hub-project-%d", i))
				require.NoError(b, err)
				_, err = setup.EnableProjectFederation(b.Context(), project.ID, "tester")
				require.NoError(b, err)
				token := fmt.Sprintf("bench-pull-token-%d", i)
				_, err = setup.CreateFederationEnrollment(b.Context(), db.CreateFederationEnrollmentParams{Token: token, SpokeInstanceUID: federationTestSpokeUID, ProjectID: &project.ID, Capabilities: "pull", Actor: "tester"})
				require.NoError(b, err)
				hubs[i] = benchHub{projectID: project.ID, token: token}
			}
			require.NoError(b, setup.Close())
			writer := newPostgresHubProcess(b, dsn)
			poller := newPostgresHubProcess(b, dsn)
			for i := range hubs {
				hubs[i].writerURL, hubs[i].pollerURL = writer.server.URL, poller.server.URL
			}
			cursors := make([]int64, count)
			for i, hub := range hubs {
				page, err := hub.pollPage(b.Context(), 0, "")
				require.NoError(b, err)
				cursors[i] = page.NextAfterID
			}
			var unrelatedReads int
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				ctx, cancel := context.WithCancel(b.Context())
				type result struct {
					page benchPollPage
					err  error
				}
				target := make(chan result, 1)
				results := make(chan error, count-1)
				before := make([]int, count)
				for i, hub := range hubs {
					before[i] = poller.reads.projectReads(hub.projectID)
					go func(h benchHub, cursor int64, first bool) {
						page, err := h.pollPage(ctx, cursor, "45")
						if first {
							target <- result{page, err}
						} else {
							results <- err
						}
					}(hub, cursors[i], i == 0)
				}
				for i, hub := range hubs {
					poller.reads.waitForProjectReads(b, hub.projectID, before[i]+1)
				}
				b.StartTimer()
				hubs[0].commit(b)
				poller.reads.waitForProjectReads(b, hubs[0].projectID, before[0]+2)
				select {
				case got := <-target:
					require.NoError(b, got.err)
					require.NotEmpty(b, got.page.Events)
					cursors[0] = got.page.NextAfterID
				case <-time.After(10 * time.Second):
					b.Fatal("target poll did not receive the committed event")
				}
				b.StopTimer()
				// Allow unrelated wakes already delivered by the bridge to reach
				// their reads before counting them.
				time.Sleep(100 * time.Millisecond) //nolint:kennlint // sleeptest: observes asynchronous wake fan-out outside the timed interval.
				for i, hub := range hubs[1:] {
					unrelatedReads += poller.reads.projectReads(hub.projectID) - before[i+1] - 1
				}
				cancel()
				for range hubs[1:] {
					<-results
				}
			}
			b.ReportMetric(float64(unrelatedReads)/float64(b.N), "unrelated-reads/op")
		})
	}
}

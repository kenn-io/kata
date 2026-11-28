package federation

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testfix"
)

// countingHub forwards requests to a hub daemon and counts them by the hub
// project ID in the request path. It skips held polls, the event polls with a
// wait: each binding keeps one open whatever the runner does, so they do not
// measure the requests a runner pass sends.
type countingHub struct {
	URL string

	mu        sync.Mutex
	total     int
	byProject map[int64]int
}

var hubProjectPath = regexp.MustCompile(`^/api/v1/projects/(\d+)/`)

func newCountingHub(t testing.TB, hubURL string) *countingHub {
	t.Helper()
	target, err := url.Parse(hubURL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	counter := &countingHub{byProject: map[int64]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("wait") {
			proxy.ServeHTTP(w, r)
			return
		}
		counter.mu.Lock()
		counter.total++
		if match := hubProjectPath.FindStringSubmatch(r.URL.Path); match != nil {
			id, _ := strconv.ParseInt(match[1], 10, 64)
			counter.byProject[id]++
		}
		counter.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	counter.URL = server.URL
	return counter
}

func (c *countingHub) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total = 0
	c.byProject = map[int64]int{}
}

func (c *countingHub) counts() (total int, byProject map[int64]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, maps.Clone(c.byProject)
}

type boundSpokeProject struct {
	hub   db.Project
	spoke db.Project
}

// bindSpokeProjects creates n federated hub projects and binds a spoke
// replica of each to hubURL, with pull and push enabled.
func bindSpokeProjects(t testing.TB, hub, spoke *sqlitestore.Store, hubURL string, n int) []boundSpokeProject {
	t.Helper()
	ctx := t.Context()
	bound := make([]boundSpokeProject, 0, n)
	for i := range n {
		hubProject, err := hub.CreateProject(ctx, fmt.Sprintf("hub-project-%02d", i))
		require.NoError(t, err)
		_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "tester")
		require.NoError(t, err)
		created, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
			Token:            fmt.Sprintf("wake-token-%02d", i),
			SpokeInstanceUID: spoke.InstanceUID(),
			ProjectID:        &hubProject.ID,
			Capabilities:     "pull,push",
			Actor:            "tester",
		})
		require.NoError(t, err)
		spokeProject, err := spoke.CreateProjectWithUID(ctx, hubProject.Name, hubProject.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID:            spokeProject.ID,
			Role:                 db.FederationRoleSpoke,
			HubURL:               hubURL,
			HubProjectID:         hubProject.ID,
			HubProjectUID:        hubProject.UID,
			ReplayHorizonEventID: 1,
			PushEnabled:          true,
			Actor:                "tester",
			Enabled:              true,
		})
		require.NoError(t, err)
		require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
			HubURL:       hubURL,
			HubProjectID: hubProject.ID,
			Token:        created.Token,
		}))
		bound = append(bound, boundSpokeProject{hub: hubProject, spoke: spokeProject})
	}
	return bound
}

type heldTimer struct{ fire chan time.Time }

func (h heldTimer) C() <-chan time.Time { return h.fire }
func (heldTimer) Stop() bool            { return true }

// newFederationNode serves a daemon over a fresh SQLite database. Unlike
// testenv.New it accepts a benchmark.
func newFederationNode(t testing.TB) (*sqlitestore.Store, string) {
	t.Helper()
	store, err := testfix.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	server := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { _ = server.Close() })
	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)
	return store, listener.URL
}

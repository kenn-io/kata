package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// hubSpokeHarness is a hub daemon reached through a counting proxy and a spoke
// store bound to one of its projects.
type hubSpokeHarness struct {
	*hubFront
	hubURL       string
	hubDB        *sqlitestore.Store
	spokeDB      *sqlitestore.Store
	hubProject   db.Project
	spokeProject db.Project
	pullToken    string
	oldHub       bool
	// watches lets the spoke see the hub's watch advertisement. Without it
	// every proxy drops the header, so the spoke holds one poll per project.
	watches atomic.Bool
	// wake wakes the runner startRunner started.
	wake chan struct{}
}

// hubFront is one proxy address for the hub, so a test can move the spoke to
// another address for the same hub.
type hubFront struct {
	url string
	// requests counts every request that reached the hub through the proxy.
	requests atomic.Int64
	// pollsWithWait and pollsWithoutWait count project event polls that
	// reached the hub with and without a wait.
	pollsWithWait    atomic.Int64
	pollsWithoutWait atomic.Int64
	// watchRequests counts project event watches that reached the hub.
	watchRequests atomic.Int64
	// held counts the polls with a wait and the watches open at the hub now;
	// peakHeld is the most that were open at once.
	heldMu   sync.Mutex
	held     int
	peakHeld int
	// holdForever keeps a poll with a wait open until its client goes away, as
	// a network interruption would.
	holdForever atomic.Bool
	// refuseHolds makes the hub answer a poll with a wait at once, as a hub at
	// its held-poll bound does.
	refuseHolds atomic.Bool
	// holdSeconds, when set, replaces the wait a poll or watch asks for and
	// the maximum the hub advertises, as a hub with a shorter maximum would.
	holdSeconds atomic.Int64
}

// newHubSpokeHarness proxies the hub. oldHub makes the hub look like one
// without long-poll support: the proxy drops the wait parameter and the
// advertisement.
func newHubSpokeHarness(tb testing.TB, oldHub bool) *hubSpokeHarness {
	tb.Helper()
	ctx := tb.Context()
	home := tb.TempDir()
	tb.Setenv("KATA_HOME", home)
	h := &hubSpokeHarness{oldHub: oldHub}
	var err error
	h.hubDB, err = sqlitestore.Open(ctx, filepath.Join(home, "hub.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = h.hubDB.Close() })
	h.spokeDB, err = sqlitestore.Open(ctx, filepath.Join(home, "spoke.db"))
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = h.spokeDB.Close() })
	hub := daemon.NewServer(daemon.ServerConfig{DB: h.hubDB, StartedAt: time.Now().UTC()})
	tb.Cleanup(func() { _ = hub.Close() })
	hubServer := httptest.NewServer(hub.Handler())
	tb.Cleanup(hubServer.Close)
	h.hubURL = hubServer.URL

	h.hubProject, err = h.hubDB.CreateProject(ctx, "hub-project")
	require.NoError(tb, err)
	var meta api.ProjectFederationBody
	h.post(tb, "/api/v1/projects/"+strconv.FormatInt(h.hubProject.ID, 10)+"/federation/enable",
		map[string]any{"actor": "tester"}, &meta)
	created, err := h.hubDB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: "delivery-pull-token", SpokeInstanceUID: h.spokeDB.InstanceUID(),
		ProjectID: &h.hubProject.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(tb, err)
	h.pullToken = created.Token
	h.hubFront = h.newFront(tb)

	h.spokeProject, err = h.spokeDB.CreateProjectWithUID(ctx, "hub-project", h.hubProject.UID)
	require.NoError(tb, err)
	_, err = h.spokeDB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: h.spokeProject.ID, Role: db.FederationRoleSpoke, HubURL: h.url,
		HubProjectID: h.hubProject.ID, HubProjectUID: h.hubProject.UID,
		ReplayHorizonEventID: meta.ReplayHorizonEventID, Enabled: true,
	})
	require.NoError(tb, err)
	require.NoError(tb, config.WriteFederationCredential(h.spokeProject.UID, config.FederationCredential{
		HubURL: h.url, HubProjectID: h.hubProject.ID, Token: h.pullToken,
	}))
	return h
}

// newFront starts another counting proxy in front of the hub.
func (h *hubSpokeHarness) newFront(tb testing.TB) *hubFront {
	tb.Helper()
	target, err := url.Parse(h.hubURL)
	require.NoError(tb, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Stopping the runner cancels its held poll; that is not worth a log line.
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	f := &hubFront{}
	proxy.ModifyResponse = func(response *http.Response) error {
		h.hideWatch(response)
		if h.oldHub {
			response.Header.Del("X-Kata-Federation-Poll-Wait-Max")
		} else if seconds := f.holdSeconds.Load(); seconds > 0 && response.Header.Get("X-Kata-Federation-Poll-Wait-Max") != "" {
			response.Header.Set("X-Kata-Federation-Poll-Wait-Max", strconv.FormatInt(seconds, 10))
		}
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if isWatch(r) || isHeldPoll(r) {
			f.heldMu.Lock()
			f.held++
			f.peakHeld = max(f.peakHeld, f.held)
			f.heldMu.Unlock()
			defer func() {
				f.heldMu.Lock()
				f.held--
				f.heldMu.Unlock()
			}()
		}
		if isWatch(r) {
			f.watchRequests.Add(1)
			if seconds := f.holdSeconds.Load(); seconds > 0 {
				query := r.URL.Query()
				query.Set("wait", strconv.FormatInt(seconds, 10))
				r.URL.RawQuery = query.Encode()
			}
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/federation/events") && r.URL.Query().Get("issue_uid") == "" {
			query := r.URL.Query()
			if !query.Has("wait") {
				f.pollsWithoutWait.Add(1)
			} else {
				f.pollsWithWait.Add(1)
				switch {
				case f.holdForever.Load():
					<-r.Context().Done()
					return
				case h.oldHub || f.refuseHolds.Load():
					query.Del("wait")
				case f.holdSeconds.Load() > 0:
					query.Set("wait", strconv.FormatInt(f.holdSeconds.Load(), 10))
				}
			}
			r.URL.RawQuery = query.Encode()
		}
		proxy.ServeHTTP(w, r)
	}))
	tb.Cleanup(server.Close)
	f.url = server.URL
	return f
}

// hideWatch drops the hub's watch advertisement unless the test opted in.
func (h *hubSpokeHarness) hideWatch(response *http.Response) {
	if !h.watches.Load() {
		response.Header.Del("X-Kata-Federation-Watch-Max")
	}
}

func isWatch(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/federation/events:watch")
}

// addBinding binds another spoke project, with its own pull enrollment, to a
// new project on the same hub through hubURL.
func (h *hubSpokeHarness) addBinding(tb testing.TB, name, hubURL string) (spoke, hub db.Project) {
	tb.Helper()
	ctx := context.WithoutCancel(tb.Context())
	hub, err := h.hubDB.CreateProject(ctx, name)
	require.NoError(tb, err)
	_, err = h.hubDB.EnableProjectFederation(ctx, hub.ID, "tester")
	require.NoError(tb, err)
	enrollment, err := h.hubDB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token: name + "-pull-token", SpokeInstanceUID: h.spokeDB.InstanceUID(), ProjectID: &hub.ID, Capabilities: "pull", Actor: "tester",
	})
	require.NoError(tb, err)
	spoke, err = h.spokeDB.CreateProjectWithUID(ctx, name, hub.UID)
	require.NoError(tb, err)
	_, err = h.spokeDB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: spoke.ID, Role: db.FederationRoleSpoke, HubURL: hubURL,
		HubProjectID: hub.ID, HubProjectUID: hub.UID, Enabled: true,
	})
	require.NoError(tb, err)
	require.NoError(tb, config.WriteFederationCredential(spoke.UID, config.FederationCredential{HubURL: hubURL, HubProjectID: hub.ID, Token: enrollment.Token}))
	return spoke, hub
}

// createIssueIn commits an issue in a hub project.
func (h *hubSpokeHarness) createIssueIn(tb testing.TB, hubProjectID int64, title string) {
	tb.Helper()
	var out map[string]any
	h.post(tb, "/api/v1/projects/"+strconv.FormatInt(hubProjectID, 10)+"/issues",
		map[string]any{"actor": "tester", "title": title}, &out)
}

// moveTo points the spoke's binding and credential at another front, as an
// endpoint change does.
func (h *hubSpokeHarness) moveTo(tb testing.TB, f *hubFront) {
	tb.Helper()
	ctx := context.WithoutCancel(tb.Context())
	binding, err := h.spokeDB.FederationBindingByProject(ctx, h.spokeProject.ID)
	require.NoError(tb, err)
	binding.HubURL = f.url
	_, err = h.spokeDB.UpsertFederationBinding(ctx, binding)
	require.NoError(tb, err)
	require.NoError(tb, config.WriteFederationCredential(h.spokeProject.UID, config.FederationCredential{
		HubURL: f.url, HubProjectID: h.hubProject.ID, Token: h.pullToken,
	}))
}

func (h *hubSpokeHarness) post(tb testing.TB, path string, body, out any) {
	tb.Helper()
	raw, err := json.Marshal(body)
	require.NoError(tb, err)
	request, err := http.NewRequestWithContext(tb.Context(), http.MethodPost, h.hubURL+path, bytes.NewReader(raw))
	require.NoError(tb, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request) //nolint:gosec // test-owned loopback hub
	require.NoError(tb, err)
	defer func() { _ = response.Body.Close() }()
	require.Equalf(tb, http.StatusOK, response.StatusCode, "POST %s", path)
	require.NoError(tb, json.NewDecoder(response.Body).Decode(out))
}

// startRunner runs the spoke's federation runner until the returned function
// stops it.
func (h *hubSpokeHarness) startRunner(tb testing.TB, interval time.Duration) func() {
	tb.Helper()
	return h.startConfiguredRunner(tb, &Runner{DB: h.spokeDB, Interval: interval})
}

// startConfiguredRunner runs runner, waking it through h.wake, until the
// returned function stops it.
func (h *hubSpokeHarness) startConfiguredRunner(tb testing.TB, runner *Runner) func() {
	tb.Helper()
	h.wake = make(chan struct{}, 1)
	runner.Wake = h.wake
	ctx, cancel := context.WithCancel(context.WithoutCancel(tb.Context()))
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	tb.Cleanup(stop)
	return stop
}

// waitForFirstPull waits until the runner's first pass has pulled from the hub.
func (h *hubSpokeHarness) waitForFirstPull(tb testing.TB) {
	tb.Helper()
	//nolint:kennlint // sleeptest: the runner pulls over real HTTP and SQLite.
	require.Eventually(tb, func() bool {
		status, err := h.spokeDB.FederationSyncStatusByProject(context.WithoutCancel(tb.Context()), h.spokeProject.ID)
		return err == nil && status.LastPullSuccessAt != nil
	}, 30*time.Second, 10*time.Millisecond)
}

func (h *hubSpokeHarness) spokeIssueCount(tb testing.TB) int {
	tb.Helper()
	var count int
	require.NoError(tb, h.spokeDB.QueryRowContext(context.WithoutCancel(tb.Context()),
		`SELECT count(*) FROM issues WHERE project_id=?`, h.spokeProject.ID).Scan(&count))
	return count
}

func (h *hubSpokeHarness) createHubIssue(tb testing.TB, title string) {
	tb.Helper()
	var out map[string]any
	h.post(tb, "/api/v1/projects/"+strconv.FormatInt(h.hubProject.ID, 10)+"/issues",
		map[string]any{"actor": "tester", "title": title}, &out)
}

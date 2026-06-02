package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/hooks"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/testfix"
)

// httptestServerHandle bundles a httptest.Server with the on-disk workspace
// directory the server was bootstrapped against. The ts field is typed as
// any to keep cross-helper imports loose; tests cast to *httptest.Server.
type httptestServerHandle struct {
	ts  any // *httptest.Server, but kept generic to avoid import cycles in helpers
	dir string
	db  *sqlitestore.Store
}

// DB returns the *sqlitestore.Store the test server is wired against, so a test can
// reach below the HTTP surface to set up state (e.g. soft-delete an issue
// before retrying create-with-idempotency-key).
func (h *httptestServerHandle) DB() *sqlitestore.Store { return h.db }

// serverOption mutates a daemon.ServerConfig before the test server starts.
// Used by helpers that wire a fresh server (e.g. newServerWithGitWorkspace,
// bootstrapProject) so callers can install non-default plumbing such as a
// recording hooks sink without having to duplicate the bootstrap dance.
type serverOption func(*daemon.ServerConfig)

// withHooksSink installs sink as the daemon's hooks.Sink so tests can observe
// every Enqueue call without spinning up the real dispatcher.
func withHooksSink(sink hooks.Sink) serverOption {
	return func(c *daemon.ServerConfig) { c.Hooks = sink }
}

// withBroadcaster installs b as the daemon's EventBroadcaster so tests can
// subscribe before invoking handlers that should broadcast mutation events.
func withBroadcaster(b *daemon.EventBroadcaster) serverOption {
	return func(c *daemon.ServerConfig) { c.Broadcaster = b }
}

// issuesURL returns the issues collection URL for the given project rowid.
func issuesURL(projectID int64) string {
	return fmt.Sprintf("/api/v1/projects/%d/issues", projectID)
}

// issueURL returns the URL for a specific issue under a project. The int64
// parameter is the issue's row id (e.g. from bootstrapProjectWithIssue);
// the helper resolves it to the issue's short_id via the active
// handle set by bootstrapProjectWithIssue. Tests that bootstrap on their
// own should call issueURLRef directly. Falls back to formatting the
// id as a string when no handle is registered (preserves negative-path
// tests like "/issues/9999" that intentionally point at no row).
func issueURL(projectID int64, issueID int64, suffix string) string {
	ref := fmt.Sprintf("%d", issueID)
	if currentHandleForIssueURL != nil {
		issue, err := currentHandleForIssueURL.DB().IssueByID(context.Background(), issueID)
		if err == nil && issue.ShortID != "" {
			ref = issue.ShortID
		}
	}
	return issueURLRef(projectID, ref, suffix)
}

// issueURLRef builds the per-issue URL from a string ref. Use this for new
// tests that hold a short_id or UID directly.
func issueURLRef(projectID int64, ref, suffix string) string {
	if suffix == "" {
		return fmt.Sprintf("/api/v1/projects/%d/issues/%s", projectID, ref)
	}
	return fmt.Sprintf("/api/v1/projects/%d/issues/%s/%s", projectID, ref, suffix)
}

// issueRefByID returns the short_id of an issue by its row id. Helper for
// tests that need to call the surface API with a wire-format ref.
func issueRefByID(t *testing.T, h *httptestServerHandle, issueID int64) string {
	t.Helper()
	issue, err := h.DB().IssueByID(context.Background(), issueID)
	require.NoError(t, err)
	return issue.ShortID
}

// linkPeerTest mirrors api.LinkPeer for test JSON decoding. UID is the
// canonical reference; ShortID is the rendered display value.
type linkPeerTest struct {
	UID     string `json:"uid"`
	ShortID string `json:"short_id"`
}

// issueShortIDFromCreate decodes the short_id field from a /issues POST
// response. Tests use this to capture the newly-minted short_id and feed it
// back as a ref to subsequent links_delta / link API calls.
func issueShortIDFromCreate(t *testing.T, bs []byte) string {
	t.Helper()
	var out struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotEmpty(t, out.Issue.ShortID, "create response missing short_id: %s", string(bs))
	return out.Issue.ShortID
}

// bootstrapProject spins up a fresh server + git workspace and runs `kata
// init` against it, returning the handle and the project rowid. Used as a
// shared setup for every issue handler test. Optional serverOptions are
// forwarded to newServerWithGitWorkspace (e.g. to install a hooks sink).
func bootstrapProject(t *testing.T, opts ...serverOption) (*httptestServerHandle, int64) {
	t.Helper()
	h := newServerWithGitWorkspace(t, "https://github.com/wesm/kata.git", opts...)
	_, bs := postJSON(t, h.ts.(*httptest.Server), "/api/v1/projects", map[string]any{"start_path": h.dir})
	var resp struct{ Project struct{ ID int64 } }
	require.NoError(t, json.Unmarshal(bs, &resp))
	// Register the handle so issueURL can look up issue.short_id at request
	// time without each test passing the handle in. Tests in this package
	// don't run in parallel, so a single pointer is enough; t.Cleanup
	// wipes it between tests.
	currentHandleForIssueURL = h
	t.Cleanup(func() {
		if currentHandleForIssueURL == h {
			currentHandleForIssueURL = nil
		}
	})
	return h, resp.Project.ID
}

// bootstrapProjectWithIssue runs bootstrapProject and additionally posts a
// minimal issue. Returns the handle, the cast *httptest.Server, the
// project rowid, and the seeded issue's row id. The fourth return value
// is treated as the issue ref in tests via issueURL — the helper looks up
// the row's short_id and substitutes it into the URL.
func bootstrapProjectWithIssue(t *testing.T) (*httptestServerHandle, *httptest.Server, int64, int64) {
	t.Helper()
	h, pid := bootstrapProject(t)
	ts := h.ts.(*httptest.Server)
	resp, bs := postJSON(t, ts, issuesURL(pid),
		map[string]any{"actor": "x", "title": "x"})
	require.Equalf(t, 200, resp.StatusCode, "seed issue: %s", string(bs))
	var body struct {
		Issue struct {
			ID int64 `json:"id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(bs, &body))
	return h, ts, pid, body.Issue.ID
}

// currentHandleForIssueURL is set by bootstrapProjectWithIssue so issueURL
// can look up an int64 issue id → short_id at request time. Tests run
// sequentially within a package by default; t.Cleanup wipes the pointer
// after each test so the global never leaks between tests.
var currentHandleForIssueURL *httptestServerHandle

// assertAPIError asserts the response carries the standard error envelope
// shape with the given HTTP status and error.code. The raw body is surfaced
// in failure messages so debugging doesn't require rerunning with -v.
func assertAPIError(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	require.Equalf(t, wantStatus, status, "status: got %d, body: %s", status, string(body))
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope), string(body))
	assert.Equal(t, wantCode, envelope.Error.Code, string(body))
}

// testDBHandle bundles a fresh *sqlitestore.Store and a started-at timestamp for daemon
// server tests.
type testDBHandle struct {
	db  *sqlitestore.Store
	now time.Time
}

// openTestDB opens a fresh sqlite DB rooted in t.TempDir() and registers a
// cleanup to close it. The returned handle is suitable for daemon.ServerConfig.
func openTestDB(t *testing.T) testDBHandle {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return testDBHandle{db: d, now: time.Now().UTC()}
}

// startTestServer wires a daemon server with the given config, mounts it on
// an httptest.Server, and registers cleanups for both. Tests that need a
// custom ServerConfig (read-only DB, hooks sink, etc.) use this directly;
// callers that just need a default server should use startDefaultTestServer.
func startTestServer(t *testing.T, cfg daemon.ServerConfig) *httptest.Server {
	t.Helper()
	srv := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// startDefaultTestServer opens a fresh test DB and starts an httptest.Server
// against it with the default ServerConfig. Returns the server and the DB
// handle so tests can inspect or mutate state below the HTTP layer.
func startDefaultTestServer(t *testing.T) (*httptest.Server, testDBHandle) {
	t.Helper()
	d := openTestDB(t)
	return startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now}), d
}

// newServerWithGitWorkspace creates a fresh git repo in t.TempDir(), wires a
// daemon server against a fresh DB, and returns a handle exposing both. When
// originURL is non-empty it is added as the "origin" remote so alias
// derivation has an http(s) URL to chew on. Optional serverOptions tweak the
// ServerConfig before the http server starts (e.g. installing a hooks sink).
func newServerWithGitWorkspace(t *testing.T, originURL string, opts ...serverOption) *httptestServerHandle {
	t.Helper()
	dir := t.TempDir()
	testfix.RunGit(t, dir, "init", "--quiet")
	if originURL != "" {
		testfix.RunGit(t, dir, "remote", "add", "origin", originURL)
	}
	d := openTestDB(t)
	cfg := daemon.ServerConfig{DB: d.db, StartedAt: d.now}
	for _, opt := range opts {
		opt(&cfg)
	}
	ts := startTestServer(t, cfg)
	return &httptestServerHandle{ts: ts, dir: dir, db: d.db}
}

// resolveProjectID posts /api/v1/projects/resolve for the given workspace dir
// and returns the resolved project rowid. Saves callers from declaring an
// anonymous struct just to extract the ID.
func resolveProjectID(t *testing.T, ts *httptest.Server, dir string) int64 {
	t.Helper()
	_, bs := postJSON(t, ts, "/api/v1/projects/resolve", map[string]any{"start_path": dir})
	var rbody struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(bs, &rbody), string(bs))
	return rbody.Project.ID
}

// postJSON issues a POST with a JSON body and returns the response plus the
// fully-drained body. The shared default for POST tests in this package.
func postJSON(t *testing.T, ts *httptest.Server, path string, body any) (*http.Response, []byte) {
	t.Helper()
	js, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(js))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, _ := io.ReadAll(resp.Body)
	return resp, bs
}

// newTestServer is a thin convenience wrapper over startDefaultTestServer for
// tests that don't need access to the underlying *sqlitestore.Store.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts, _ := startDefaultTestServer(t)
	return ts
}

// patchJSON issues a PATCH request with a JSON body and returns the response
// plus the buffered body. Mirrors postJSON for the PATCH-only handlers.
func patchJSON(t *testing.T, ts *httptest.Server, path string, body any) (*http.Response, []byte) {
	t.Helper()
	js, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPatch, ts.URL+path, bytes.NewReader(js))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // test request to httptest server URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, _ := io.ReadAll(resp.Body)
	return resp, bs
}

// doReq builds and executes an arbitrary-method HTTP request against ts. When
// body is non-nil it is JSON-encoded and Content-Type is set automatically;
// extra headers are layered on top. The response body is fully drained and
// returned, and resp.Body is closed before the function returns.
func doReq(t *testing.T, ts *httptest.Server, method, path string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		js, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(js)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, bodyReader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // test request to httptest server URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, bs
}

// archiveProject soft-deletes (archives) a project via the DB layer. Pass
// force=true to bypass the open-issues guard when the project still has live
// issues. Tests use this to set up the archived-project state without going
// through the HTTP DELETE path.
func archiveProject(t *testing.T, h *httptestServerHandle, projectID int64, force bool) {
	t.Helper()
	_, _, err := h.DB().RemoveProject(context.Background(), db.RemoveProjectParams{
		ProjectID: projectID,
		Actor:     "tester",
		Force:     force,
	})
	require.NoError(t, err)
}

// getBody runs a GET against the test server and asserts a 2xx status. Returns
// the body as a string for easy substring assertions.
func getBody(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test request to httptest server URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(bs))
	return string(bs)
}

// getAndUnmarshal performs a GET against ts, asserts the response status
// matches wantStatus, and decodes the body into target. The raw body is
// surfaced in failure messages so debugging doesn't require rerunning.
func getAndUnmarshal(t *testing.T, ts *httptest.Server, path string, wantStatus int, target any) {
	t.Helper()
	resp, bs := getStatusBody(t, ts, path)
	require.Equalf(t, wantStatus, resp.StatusCode, "GET %s: status %d, body: %s", path, resp.StatusCode, string(bs))
	require.NoError(t, json.Unmarshal(bs, target), string(bs))
}

// getStatusBody is like getBody but returns the response so callers can assert
// on non-2xx status codes.
func getStatusBody(t *testing.T, ts *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	require.NoError(t, err)
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test request to httptest server URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	bs, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, bs
}

// httpResp is a status+body pair captured by postWithHeader. The bytes are
// already drained, so callers can read the body multiple times.
type httpResp struct {
	status int
	body   []byte
}

// postWithHeader is like postJSON but allows setting custom headers (e.g. the
// Idempotency-Key header tested by the createIssue handler).
func postWithHeader(t *testing.T, ts *httptest.Server, path string, headers map[string]string, body any) httpResp {
	t.Helper()
	bs, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+path, bytes.NewReader(bs))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req) //nolint:gosec // G704: test request to httptest server URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResp{status: resp.StatusCode, body: out}
}

// requireOK asserts that the captured response was a 200 OK; surfaces the body
// in the failure message so callers don't need to repeat the wrap.
func requireOK(t *testing.T, r httpResp) {
	t.Helper()
	require.Equalf(t, 200, r.status, "expected 200, got %d: %s", r.status, string(r.body))
}

// receiveMsg blocks until a StreamMsg arrives on ch or timeout fires; on
// timeout it calls t.Fatalf with the supplied label so failures identify the
// site without a custom select block.
func receiveMsg(t *testing.T, ch <-chan daemon.StreamMsg, timeout time.Duration, label string) daemon.StreamMsg {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(timeout):
		t.Fatalf("%s: did not receive within %s", label, timeout)
		return daemon.StreamMsg{}
	}
}

// assertNoReceive asserts that no StreamMsg arrives on ch within the timeout.
// A delivery is reported via t.Fatalf, surfacing the offending message.
func assertNoReceive(t *testing.T, ch <-chan daemon.StreamMsg, timeout time.Duration, label string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s: unexpected delivery %+v", label, got)
	case <-time.After(timeout):
	}
}

// assertChannelClosed waits up to timeout for ch to be closed, draining any
// values delivered in the meantime. Any value received before the close is
// discarded — callers that care about delivered values should drain explicitly.
func assertChannelClosed(t *testing.T, ch <-chan daemon.StreamMsg, timeout time.Duration, label string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-time.After(remaining):
		}
	}
	t.Fatalf("%s: channel was not closed within %s", label, timeout)
}

// broadcastEvent constructs a synthetic issue.created event with the given
// IDs and broadcasts it through b. Saves callers the StreamMsg{Kind:"event"}
// boilerplate when the specific event payload doesn't matter.
func broadcastEvent(b *daemon.EventBroadcaster, projectID, eventID int64) {
	evt := &db.Event{ID: eventID, ProjectID: projectID, Type: "issue.created"}
	b.Broadcast(daemon.StreamMsg{Kind: "event", Event: evt, ProjectID: projectID})
}

// doReqEnv is the workhorse for testenv-based HTTP helpers. It sets the
// auth bearer (the testenv default token), Content-Type when a body is
// present, and an If-Match header when ifMatch is non-empty.
func doReqEnv(t *testing.T, env *testenv.Env, method, url, body, ifMatch string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer tok")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := env.HTTP.Do(req) //nolint:gosec // G704: test server URL, not user-controlled
	require.NoError(t, err)
	return resp
}

func doGet(t *testing.T, env *testenv.Env, url string) *http.Response {
	return doReqEnv(t, env, http.MethodGet, url, "", "")
}

func doPost(t *testing.T, env *testenv.Env, url, body string) *http.Response {
	return doReqEnv(t, env, http.MethodPost, url, body, "")
}

func doPostWithIfMatch(t *testing.T, env *testenv.Env, url, body, ifMatch string) *http.Response {
	return doReqEnv(t, env, http.MethodPost, url, body, ifMatch)
}

func doPatch(t *testing.T, env *testenv.Env, url, body, ifMatch string) *http.Response {
	return doReqEnv(t, env, http.MethodPatch, url, body, ifMatch)
}

func doDelete(t *testing.T, env *testenv.Env, url string) *http.Response {
	return doReqEnv(t, env, http.MethodDelete, url, "", "")
}

// seedProject creates a project named name via the DB and asserts no error.
// Lives here instead of per-file so handler tests in different domains share
// the same fixture.
func seedProject(t *testing.T, env *testenv.Env, name string) db.Project {
	t.Helper()
	p, err := env.DB.CreateProject(t.Context(), name)
	require.NoError(t, err)
	return p
}

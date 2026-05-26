package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonclient"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/version"
)

// setupKataEnv points KATA_HOME and KATA_DB at a fresh temp dir so the test
// runs in isolation from any developer-local state. Returns the temp dir.
func setupKataEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	return tmp
}

// executeRoot runs the given cobra command with args and returns stdout. It
// fails the test if Execute returns an error. Use for non-workspace tests
// (daemon, etc.); workspace-bound tests should use runCLI instead.
func executeRoot(t *testing.T, cmd *cobra.Command, args ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	require.NoError(t, cmd.Execute())
	return buf.Bytes()
}

// executeRootCapture runs a fresh root cobra command with the given context
// and args, returning captured stdout, stderr, and any Execute error. Unlike
// executeRoot it does not fail on error — callers decide how to react (assert
// success, expect a specific error, or ignore for tail/timeout cases).
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func executeRootCapture(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var so, se bytes.Buffer
	cmd.SetOut(&so)
	cmd.SetErr(&se)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err = cmd.Execute()
	if err != nil {
		emitRootError(&se, cmd, args, err, runEEntered)
	}
	return so.String(), se.String(), err
}

// pipeServer starts a TCP listener on a random loopback port, registers
// GET /api/v1/ping, and returns the host:port address and a cleanup function.
func pipeServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pipeServer: listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"service": "kata",
			"version": version.Version,
			"pid":     os.Getpid(),
		})
	})
	go func() { _ = http.Serve(l, mux) }() //nolint:gosec // test-only, loopback only
	return l.Addr().String(), func() { _ = l.Close() }
}

// writeRuntimeFor writes a daemon.<pid>.json inside the namespace DataDir that
// resolves from the given KATA_HOME (tmp). The test must have already called
// t.Setenv("KATA_HOME", tmp) and t.Setenv("KATA_DB", ...) before this.
func writeRuntimeFor(home, addr string) error {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return err
	}
	if err := ns.EnsureDirs(); err != nil {
		return err
	}
	rec := daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Address:   addr,
		DBPath:    home + "/kata.db",
		StartedAt: time.Now().UTC(),
	}
	_, err = daemon.WriteRuntimeFile(ns.DataDir, rec)
	return err
}

// contextWithBaseURL injects a daemon base URL into the context so CLI
// commands bypass real daemon discovery during tests.
func contextWithBaseURL(ctx context.Context, url string) context.Context {
	return context.WithValue(ctx, daemonclient.BaseURLKey{}, url)
}

// runGit executes `git <args>` in dir and fails the test on error. Use for
// test setup that needs to initialize a workspace, configure remotes, etc.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // git binary is trusted; args are test-controlled
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
}

// postJSON marshals reqBody, POSTs to url, asserts a 200 response, and decodes
// the JSON body into Resp. Use for daemon API helpers that read the response.
func postJSON[Resp any](t *testing.T, url string, reqBody any) Resp {
	t.Helper()
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, 200, resp.StatusCode, "POST %s", url)
	var out Resp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// postJSONOK is postJSON without a response body: marshal, POST, assert 200,
// discard. Use for daemon API helpers that don't care about the response.
func postJSONOK(t *testing.T, url string, reqBody any) {
	t.Helper()
	body, err := json.Marshal(reqBody)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, 200, resp.StatusCode, "POST %s", url)
}

// getJSON GETs url, asserts a 200 response, and decodes the body into Resp.
func getJSON[Resp any](t *testing.T, url string) Resp {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx,gosec // test-only loopback
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equalf(t, 200, resp.StatusCode, "GET %s", url)
	var out Resp
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// initBoundWorkspace creates a temporary git workspace, adds a git remote, and
// registers it with the test daemon via POST /api/v1/projects. Returns the
// workspace directory path.
func initBoundWorkspace(t *testing.T, baseURL, origin string) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", origin)
	postJSONOK(t, baseURL+"/api/v1/projects", map[string]string{"start_path": dir})
	return dir
}

// resolvePIDViaHTTP calls POST /api/v1/projects/resolve with start_path and
// returns the resolved project ID.
func resolvePIDViaHTTP(t *testing.T, baseURL, startPath string) int64 {
	t.Helper()
	type response struct {
		Project struct{ ID int64 } `json:"project"`
	}
	b := postJSON[response](t, baseURL+"/api/v1/projects/resolve", map[string]string{"start_path": startPath})
	return b.Project.ID
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// createdIssue captures the wire fields tests need after creating an issue:
// the short_id (for ref-bearing URLs and human assertions), the UID (for
// ULID-form refs), and the database row id (for low-level DB lookups in
// tests that bypass the API).
type createdIssue struct {
	ID      int64
	UID     string
	ShortID string
}

// createIssueViaHTTP creates an issue in dir's project via the testenv daemon.
// Returns the short_id from the response. Reused across destructive-ladder
// tests so each test doesn't have to resolve the project ID itself.
func createIssueViaHTTP(t *testing.T, env *testenv.Env, dir, title string) string {
	t.Helper()
	return createIssueViaHTTPFull(t, env, dir, title).ShortID
}

// createIssueViaHTTPFull is the same as createIssueViaHTTP but returns every
// identifier the test might need (row id, UID, short_id).
func createIssueViaHTTPFull(t *testing.T, env *testenv.Env, dir, title string) createdIssue {
	t.Helper()
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	type response struct {
		Issue struct {
			ID      int64  `json:"id"`
			UID     string `json:"uid"`
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	b := postJSON[response](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues",
		map[string]any{"actor": "tester", "title": title})
	return createdIssue{ID: b.Issue.ID, UID: b.Issue.UID, ShortID: b.Issue.ShortID}
}

// resetFlags restores global flag state for cobra tests. Use t.Cleanup so
// LIFO ordering plays nicely with other cleanups.
func resetFlags(t *testing.T) {
	t.Helper()
	saved := flags
	flags = globalFlags{}
	t.Cleanup(func() { flags = saved })
}

// stubIsTTY swaps the package-level isTTY hook for a constant-valued stub
// during the test. Required for the noninteractive-stdin branch of
// resolveConfirm: `go test` from a developer's terminal otherwise sees the
// real stdin as a TTY, which would make the assertion flaky.
func stubIsTTY(t *testing.T, want bool) {
	t.Helper()
	saved := isTTY
	isTTY = func(*os.File) bool { return want }
	t.Cleanup(func() { isTTY = saved })
}

// runCmdOutput executes a fresh root command with args and returns captured
// stdout plus any Execute error. resetFlags is applied. If env is non-nil the
// daemon base URL is injected into the context. Use for non-workspace tests
// that need the daemon URL but not the --workspace flag.
func runCmdOutput(t *testing.T, env *testenv.Env, args ...string) (string, error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs(args)
	ctx := context.Background()
	if env != nil {
		ctx = contextWithBaseURL(ctx, env.URL)
	}
	cmd.SetContext(ctx)
	err := cmd.Execute()
	return buf.String(), err
}

// requireCmdOutput is runCmdOutput with require.NoError on the result.
func requireCmdOutput(t *testing.T, env *testenv.Env, args ...string) string {
	t.Helper()
	out, err := runCmdOutput(t, env, args...)
	require.NoError(t, err)
	return out
}

// setupMergeProjects creates the canonical "alpha" and "beta" projects, each
// with a matching git alias, used by ProjectsMerge tests.
func setupMergeProjects(t *testing.T, env *testenv.Env) (alpha, beta db.Project) {
	t.Helper()
	ctx := context.Background()
	var err error
	alpha, err = env.DB.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err = env.DB.CreateProject(ctx, "beta")
	require.NoError(t, err)
	_, err = env.DB.AttachAlias(ctx, alpha.ID, "github.com/wesm/alpha", "git", "/tmp/alpha")
	require.NoError(t, err)
	_, err = env.DB.AttachAlias(ctx, beta.ID, "github.com/wesm/beta", "git", "/tmp/beta")
	require.NoError(t, err)
	return alpha, beta
}

// runCLI executes a root command and returns the output.
func runCLI(t *testing.T, env *testenv.Env, dir string, args ...string) string {
	t.Helper()
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	fullArgs := append([]string{"--workspace", dir}, args...)
	cmd.SetArgs(fullArgs)
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	require.NoError(t, cmd.Execute())
	return strings.TrimSpace(buf.String())
}

// runCLIAs is runCLI with an explicit actor. It threads `--as <actor>`
// through so each invocation records the named actor rather than the
// test process's resolved default. Use when a test needs distinct
// actors on otherwise identical commands (e.g. audit filter-by-actor).
func runCLIAs(t *testing.T, env *testenv.Env, dir, actor string, args ...string) string {
	t.Helper()
	return runCLI(t, env, dir, append([]string{"--as", actor}, args...)...)
}

// runCLICapture is the error-tolerant sibling of runCLI: it runs a fresh root
// command bound to dir under env's daemon URL and returns combined
// stdout+stderr plus the Execute error. Use when a test needs to assert on a
// specific failure (cliError code, exit status) instead of just succeeding.
func runCLICapture(t *testing.T, env *testenv.Env, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--workspace", dir}, args...))
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	err := cmd.Execute()
	return buf.String(), err
}

// runCLIWithErr is runCLICapture with stdout and stderr captured separately.
// On Execute failure the error message is also written to the stderr buffer
// (mirroring main.go's emitError for the human path) so tests can assert on
// stderr as the user would see it.
func runCLIWithErr(t *testing.T, env *testenv.Env, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var so, se bytes.Buffer
	cmd.SetOut(&so)
	cmd.SetErr(&se)
	cmd.SetArgs(append([]string{"--workspace", dir}, args...))
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	err = cmd.Execute()
	if err != nil {
		emitError(&se, err, false, true)
	}
	return so.String(), se.String(), err
}

// setupWorkspaceWithIssue initializes a test environment and workspace with
// a single issue. Returns the environment, workspace dir, project ID, and the
// created issue's short_id (suitable for use as a positional ref).
func setupWorkspaceWithIssue(t *testing.T, issueTitle string) (*testenv.Env, string, int64, string) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, issueTitle)
	return env, dir, pid, short
}

// setupCLIWorkspace bundles resetFlags, testenv.New, initBoundWorkspace, and
// project ID resolution into one call. Use for CLI tests that seed their own
// issues; setupWorkspaceWithIssue is the one-issue convenience wrapper.
func setupCLIWorkspace(t *testing.T) (*testenv.Env, string, int64) {
	t.Helper()
	resetFlags(t)
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	return env, dir, pid
}

// createIssue creates an issue via HTTP and returns its short_id. Tests can
// use the short_id directly as the positional ref on subsequent CLI calls.
func createIssue(t *testing.T, env *testenv.Env, projectID int64, title string) string {
	t.Helper()
	type response struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	b := postJSON[response](t, env.URL+"/api/v1/projects/"+itoa(projectID)+"/issues",
		map[string]any{"actor": "tester", "title": title})
	return b.Issue.ShortID
}

// createLinkViaHTTP creates a link between two issues via HTTP. Used by tests
// that need a pre-existing link to exercise unlink/show/etc. Refs are
// short_ids or ULIDs (anything the daemon's {ref} path accepts).
func createLinkViaHTTP(t *testing.T, env *testenv.Env, projectID int64, fromRef, linkType, toRef string) {
	t.Helper()
	postJSONOK(t,
		env.URL+"/api/v1/projects/"+itoa(projectID)+"/issues/"+fromRef+"/links",
		map[string]any{"actor": "tester", "type": linkType, "to_ref": toRef})
}

// setupCLIEnv combines the standard workspace initialization and server startup.
func setupCLIEnv(t *testing.T) (*testenv.Env, string) {
	t.Helper()
	resetFlags(t)
	env := testenv.New(t)
	dir := initBoundWorkspace(t, env.URL, "https://github.com/wesm/kata.git")
	return env, dir
}

// cliFixture bundles a workspace-bound test environment with a fresh root
// command whose stdout/stderr write into a single shared buffer and whose
// context already carries the test daemon's base URL. Use newCLIFixture for
// CLI tests that need to inspect output or expect specific errors; runCLI is
// preferable for plain success-path checks.
type cliFixture struct {
	env *testenv.Env
	dir string
	cmd *cobra.Command
	buf *bytes.Buffer
}

// newCLIFixture sets up resetFlags, a fresh testenv, a bound workspace, and a
// new root command wired to a shared buffer and the daemon base URL context.
func newCLIFixture(t *testing.T) *cliFixture {
	t.Helper()
	env, dir := setupCLIEnv(t)
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(contextWithBaseURL(context.Background(), env.URL))
	return &cliFixture{env: env, dir: dir, cmd: cmd, buf: &buf}
}

// execute runs the fixture's command with --workspace prepended to args.
func (f *cliFixture) execute(args ...string) error {
	f.cmd.SetArgs(append([]string{"--workspace", f.dir}, args...))
	return f.cmd.Execute()
}

// rootSubcommands returns the freshly constructed root command's subcommands
// keyed by name. Use to assert subcommand registration without iterating
// cmd.Commands() in every advertise test.
func rootSubcommands() map[string]*cobra.Command {
	cmd := newRootCmd()
	out := make(map[string]*cobra.Command, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		out[sub.Name()] = sub
	}
	return out
}

// errorEnvelopeForTest mirrors the JSON envelope emitted by emitError under
// --json. Tests assert on Kind/Code/Message/ExitCode; fields the test does not
// care about decode to their zero value.
type errorEnvelopeForTest struct {
	Error struct {
		Kind     string `json:"kind"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
}

// parseErrorEnvelope unmarshals a --json error payload, failing the test with
// the raw bytes if the payload isn't parseable JSON.
func parseErrorEnvelope(t *testing.T, data []byte) errorEnvelopeForTest {
	t.Helper()
	var got errorEnvelopeForTest
	require.NoError(t, json.Unmarshal(data, &got),
		"--json error must be parseable JSON; got %q", string(data))
	return got
}

// requireCLIError asserts that err is a *cliError with the given exit code and
// returns it for further code/message inspection by the caller.
func requireCLIError(t *testing.T, err error, expectedCode int) *cliError {
	t.Helper()
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, expectedCode, ce.ExitCode)
	return ce
}

// linkPeerTest mirrors api.LinkPeer for test decoding.
type linkPeerTest struct {
	UID     string `json:"uid"`
	ShortID string `json:"short_id"`
}

type IssueResponse struct {
	Issue struct {
		ID       int64   `json:"id"`
		ShortID  string  `json:"short_id"`
		UID      string  `json:"uid"`
		Title    string  `json:"title"`
		Owner    *string `json:"owner"`
		Priority *int64  `json:"priority"`
		Status   string  `json:"status"`
	} `json:"issue"`
	Labels []struct {
		Label string `json:"label"`
	} `json:"labels"`
	Links []struct {
		Type string       `json:"type"`
		From linkPeerTest `json:"from"`
		To   linkPeerTest `json:"to"`
	} `json:"links"`
	Comments []struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	} `json:"comments"`
}

// fetchIssueViaHTTPWithComments is fetchIssueViaHTTP — it already returns the
// Comments field on IssueResponse — kept as a named alias so tests reading
// comments document their intent and survive future shape changes.
func fetchIssueViaHTTPWithComments(t *testing.T, env *testenv.Env, pid int64, ref string) IssueResponse {
	t.Helper()
	return fetchIssueViaHTTP(t, env, pid, ref)
}

// safeBuffer is a mutex-protected bytes.Buffer used by streaming/tail tests so
// `go test -race` does not flag the goroutine running cmd.Execute writing to
// the buffer racing with the test goroutine reading it via Snapshot.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) Snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// asyncCLI wraps a long-running cobra command executing in a background
// goroutine. Use awaitOutput to poll the captured stdout/stderr for expected
// substrings and stop to cancel the derived context and wait for completion.
type asyncCLI struct {
	buf    *safeBuffer
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startAsyncCLI builds a fresh root command with args, attaches a safeBuffer to
// stdout/stderr, and runs Execute in a background goroutine under a context
// derived from ctx. resetFlags is applied. Callers stop it via stop().
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func startAsyncCLI(t *testing.T, ctx context.Context, args ...string) *asyncCLI {
	t.Helper()
	resetFlags(t)
	derived, cancel := context.WithCancel(ctx)
	cmd := newRootCmd()
	buf := &safeBuffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	cmd.SetContext(derived)
	a := &asyncCLI{buf: buf, cancel: cancel}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		_ = cmd.Execute()
	}()
	return a
}

// awaitOutput polls the snapshot until predicate returns true or timeout
// expires. Returns the final snapshot in either case so callers can assert on
// the captured output.
func (a *asyncCLI) awaitOutput(predicate func(string) bool, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate(a.buf.Snapshot()) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return a.buf.Snapshot()
}

// stop cancels the derived context and waits for Execute to return.
func (a *asyncCLI) stop() {
	a.cancel()
	a.wg.Wait()
}

func fetchIssueViaHTTP(t *testing.T, env *testenv.Env, pid int64, ref string) IssueResponse {
	t.Helper()
	return getJSON[IssueResponse](t, env.URL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
}

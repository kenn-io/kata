//go:build !windows

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	kitdaemon "go.kenn.io/kit/daemon"
)

// safeBuffer is a bytes.Buffer guarded by a mutex. os/exec writes the
// child's stderr from a background goroutine, while the test reads
// String() for failure messages — concurrent access without locking
// trips the race detector.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestE2E_Hooks_IssueCreatedTriggersHook spawns a real `kata daemon`
// subprocess, drops a hooks.toml that runs a tiny shell script writing
// $KATA_EVENT_ID to a marker file, then verifies:
//   - the marker file appears (the hook actually executed)
//   - runs.jsonl gained a line with result=ok
//   - `kata daemon logs --hooks` surfaces the same record
//
// Unlike TestSmoke_* (which uses an in-process httptest server), this
// test exercises the real subprocess wiring: signal handling, runtime
// file, unix socket transport, dispatcher shutdown.
func TestE2E_Hooks_IssueCreatedTriggersHook(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests are slow")
	}

	dirs := newE2EDirs(t)
	writeHookScript(t, dirs)

	bin := buildKataBinary(t)
	env := dirs.env()

	daemonStderr := startDaemon(t, bin, env)

	url, client := connectDaemon(t, dirs, daemonStderr)

	pid := initProjectE2E(t, client, url, dirs.repoDir)
	createIssueE2E(t, client, url, pid, daemonStderr)

	eventID := waitForMarker(t, dirs.marker, daemonStderr)
	require.NotEmpty(t, eventID, "KATA_EVENT_ID was empty in marker")

	verifyRunsJSONL(t, dirs)
	verifyDaemonLogsHooks(t, bin, env)
}

// e2eDirs bundles the per-test paths and file locations used across the
// helpers. It keeps the top-level test free of filepath.Join noise and
// makes sure marker, hook script, and home are constructed consistently.
type e2eDirs struct {
	home    string
	repoDir string
	dbPath  string
	marker  string
	script  string
	xdgDir  string
}

func newE2EDirs(t *testing.T) e2eDirs {
	t.Helper()
	home := t.TempDir()
	repoDir := t.TempDir()
	require.NoError(t, runIn(repoDir, "git", "init", "--quiet"))
	require.NoError(t, runIn(repoDir,
		"git", "remote", "add", "origin", "https://github.com/wesm/kata-e2e.git"))
	// Unix socket paths cap at 104 chars on macOS / 108 on Linux. The
	// per-test t.TempDir() under macOS's /var/folders blows past that
	// once /kata/<dbhash>/daemon.sock is appended, so XDG_RUNTIME_DIR
	// must be a short path. /tmp is short on both macOS and Linux,
	// and the cleanup removes the dir when the test ends.
	xdg, err := os.MkdirTemp("/tmp", "kata-e2e-xdg-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	return e2eDirs{
		home:    home,
		repoDir: repoDir,
		dbPath:  filepath.Join(home, "kata.db"),
		marker:  filepath.Join(home, "marker.txt"),
		script:  filepath.Join(home, "hook.sh"),
		xdgDir:  xdg,
	}
}

func (d e2eDirs) env() []string {
	return append(os.Environ(),
		"KATA_HOME="+d.home,
		"KATA_DB="+d.dbPath,
		"XDG_RUNTIME_DIR="+d.xdgDir,
		"KATA_AUTH_TOKEN=",
		"KATA_TRUST_PRIVATE_NETWORK=",
		"KATA_SERVER=",
	)
}

// writeHookScript drops a tiny shell hook that records $KATA_EVENT_ID
// to the marker file, plus the hooks.toml that wires the script to
// issue.created. The script is owner-rwx (0o700) so /bin/sh can run
// it; gosec's G306 is suppressed because the executable bit is
// required, not optional.
func writeHookScript(t *testing.T, d e2eDirs) {
	t.Helper()
	body := "#!/bin/sh\nprintf '%s' \"$KATA_EVENT_ID\" > " + shellQuote(d.marker) + "\n"
	//nolint:gosec // G306: hook script needs the executable bit set so /bin/sh can run it.
	require.NoError(t, os.WriteFile(d.script, []byte(body), 0o700))

	tomlBody := fmt.Sprintf(
		"[[hook]]\nevent = \"issue.created\"\ncommand = %q\nworking_dir = %q\n",
		d.script, d.home,
	)
	require.NoError(t, os.WriteFile(filepath.Join(d.home, "hooks.toml"), []byte(tomlBody), 0o600))
}

// shellQuote single-quotes a path for embedding in a /bin/sh command.
// Single quotes are inert inside POSIX sh, so embedded special chars
// (spaces, $, etc.) come through untouched. Embedded single quotes are
// closed-then-escaped-then-reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildKataBinary compiles `cmd/kata` into a fresh temp dir and returns
// the absolute path. A separate temp dir keeps build artifacts out of
// $KATA_HOME so the marker file and runs.jsonl can't collide.
func buildKataBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kata")
	build := exec.Command("go", "build", "-o", bin, "go.kenn.io/kata/cmd/kata") //nolint:gosec // G204: fixed args, test-only
	var stderr bytes.Buffer
	build.Stderr = &stderr
	require.NoErrorf(t, build.Run(), "go build kata: %s", stderr.String())
	return bin
}

// startDaemon spawns `kata daemon start` in its own process group and
// registers a t.Cleanup that SIGTERMs (then SIGKILLs after 2s) so a
// stuck daemon never leaks across tests. The captured stderr buffer is
// returned so failure paths can include daemon logs in their messages.
func startDaemon(t *testing.T, bin string, env []string) *safeBuffer {
	t.Helper()
	stderr := &safeBuffer{}
	//nolint:gosec // G204: bin is buildKataBinary's output
	cmd := exec.Command(bin, "daemon", "start")
	cmd.Env = env
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		stopDaemon(cmd)
	})
	return stderr
}

// stopDaemon sends SIGTERM and waits up to 2s for the process to exit;
// after that it escalates to SIGKILL. Errors are ignored because the
// daemon may already have exited (a nil-Process check guards both
// signal calls).
func stopDaemon(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// connectDaemon polls the runtime dir for the daemon.<pid>.json file,
// extracts the unix-socket address, and returns a base URL ("http://kata.invalid")
// + a unix-socket-aware HTTP client. Fails the test if the daemon never
// advertises an address within 10s.
func connectDaemon(t *testing.T, d e2eDirs, daemonStderr *safeBuffer) (string, *http.Client) {
	t.Helper()
	runtimeDir := filepath.Join(d.home, "runtime", config.DBHash(d.dbPath))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sockPath, ok := readDaemonSocketPath(runtimeDir)
		if ok {
			client := &http.Client{
				Transport: newUnixTransport(sockPath),
				Timeout:   5 * time.Second,
			}
			if pingDaemon(client) {
				return "http://kata.invalid", client
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("daemon never advertised a unix socket in %s\ndaemon stderr: %s",
		runtimeDir, daemonStderr.String())
	return "", nil
}

// readDaemonSocketPath scans runtimeDir for a daemon.<pid>.json file
// and returns the unix-socket path it advertises. Returns ("", false)
// when no parsable file exists yet, or when the file points at a
// non-unix endpoint (the test only supports unix sockets — TCP would
// require a separate dial path).
func readDaemonSocketPath(runtimeDir string) (string, bool) {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "daemon.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(runtimeDir, name)) //nolint:gosec // path under test tempdir
		if err != nil {
			continue
		}
		var rec kitdaemon.RuntimeRecord
		if err := json.Unmarshal(body, &rec); err != nil {
			continue
		}
		ep := rec.Endpoint()
		if ep.IsUnix() {
			return ep.Address, true
		}
	}
	return "", false
}

// pingDaemon issues GET /api/v1/ping and returns true on a 200. Any
// other status (including transport errors during socket-listen race)
// is treated as not-yet-ready.
func pingDaemon(client *http.Client) bool {
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "http://kata.invalid/api/v1/ping", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// newUnixTransport builds an *http.Transport whose DialContext targets
// a Unix socket. Mirrors cmd/kata/client.go's helper of the same name.
func newUnixTransport(path string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}
}

// initProjectE2E posts /api/v1/projects with the test's git repo and
// returns the resolved project ID. The repo has a remote so the daemon
// can derive a project name from the git URL.
func initProjectE2E(t *testing.T, client *http.Client, baseURL, repoDir string) int64 {
	t.Helper()
	body := strings.NewReader(`{"start_path":"` + repoDir + `"}`)
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, baseURL+"/api/v1/projects", body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "init project: %s", raw)
	var pl struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(raw, &pl), "init response: %s", raw)
	require.NotZero(t, pl.Project.ID, "init must return a project id: %s", raw)
	return pl.Project.ID
}

// createIssueE2E posts /api/v1/projects/{pid}/issues. The daemonStderr
// buffer is interpolated into the failure message so a hook config or
// dispatcher error from the daemon side is visible without re-running.
func createIssueE2E(t *testing.T, client *http.Client, baseURL string, pid int64, daemonStderr *safeBuffer) {
	t.Helper()
	url := baseURL + "/api/v1/projects/" + strconv.FormatInt(pid, 10) + "/issues"
	body := strings.NewReader(`{"actor":"tester","title":"e2e"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req) //nolint:gosec // G704: test-only unix socket, fixed URL
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, resp.StatusCode,
		"create issue: status=%d body=%s daemon stderr=%s",
		resp.StatusCode, raw, daemonStderr.String())
}

// waitForMarker polls the marker file written by the hook script. It
// returns the trimmed contents (the issue.created event ID). Failures
// include daemon stderr so a queue-full or spawn error is visible.
func waitForMarker(t *testing.T, marker string, daemonStderr *safeBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(marker) //nolint:gosec // path under test tempdir
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("hook never fired (no marker file). daemon stderr: %s", daemonStderr.String())
	return ""
}

// verifyRunsJSONL polls runs.jsonl until it gains content, then asserts
// the first record's result field is "ok". The hook ran successfully
// once the marker exists, but the runs file write happens asynchronously
// after the dispatcher's runner finalizes the record.
func verifyRunsJSONL(t *testing.T, d e2eDirs) {
	t.Helper()
	dbHash := config.DBHash(d.dbPath)
	runsPath := filepath.Join(d.home, "hooks", dbHash, "runs.jsonl")
	deadline := time.Now().Add(2 * time.Second)
	var content []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(runsPath) //nolint:gosec // path under test tempdir
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			content = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NotEmpty(t, content, "runs.jsonl never gained content at %s", runsPath)
	first := bytes.SplitN(content, []byte("\n"), 2)[0]
	var rec map[string]any
	require.NoError(t, json.Unmarshal(first, &rec), "runs.jsonl line: %s", first)
	require.Equal(t, "ok", rec["result"], "first run record should be result=ok: %v", rec)
}

// verifyDaemonLogsHooks runs `kata daemon logs --hooks` as a subprocess
// and checks that it surfaces the run record. This proves the
// file-direct read path (orderedRunsFiles) works without daemon
// roundtrips, which is the design intent of `daemon logs --hooks`.
func verifyDaemonLogsHooks(t *testing.T, bin string, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:gosec // G204: bin is the test-built binary
	logs := exec.CommandContext(ctx, bin, "daemon", "logs", "--hooks")
	logs.Env = env
	out, err := logs.Output()
	require.NoErrorf(t, err, "daemon logs --hooks failed: %s", out)
	require.Contains(t, string(out), `"result":"ok"`,
		"daemon logs --hooks output missing run line: %s", out)
}

// runIn runs the named command in dir, discarding stdout/stderr. Used
// by initRepo-style helpers to set up a real git repo so the daemon's
// alias resolution has something to chew on.
func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // test-controlled args
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

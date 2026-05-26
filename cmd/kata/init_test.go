package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestInit_FreshGitRepoBindsViaRemote(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	resetFlags(t)
	flags.JSON = true

	ctx := context.Background()
	out, err := callInit(ctx, env.URL, dir, callInitOpts{})
	require.NoError(t, err)
	assert.Contains(t, out, `"name":"kata"`)
	assert.NotContains(t, out, `"identity":`)
	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
}

func TestInit_AddsLocalToGitignoreWhenAbsent(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, ".gitignore")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), ".kata.local.toml")
}

func TestInit_GitignoreIsIdempotent(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), //nolint:gosec // test fixture mirrors production .gitignore mode
		[]byte("node_modules/\n.kata.local.toml\n"), 0o644))

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, ".gitignore")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	// Exactly one occurrence — no duplication on re-run.
	assert.Equal(t, 1, strings.Count(string(content), ".kata.local.toml"))
	assert.Contains(t, string(content), "node_modules/")
}

// TestInit_GitignoreLandsAtWorkspaceRoot exercises the nested-init case:
// when `kata init` runs from a subdirectory of the git workspace, the
// daemon writes .kata.toml at the git root and reports that root in
// workspace_root. The CLI must place .gitignore beside .kata.toml at
// the workspace root, not at the cwd subdirectory.
func TestInit_GitignoreLandsAtWorkspaceRoot(t *testing.T) {
	env := testenv.New(t)
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	sub := filepath.Join(root, "internal", "tui")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, sub, callInitOpts{})
	require.NoError(t, err)

	// .kata.toml is written by the daemon at the git root, not the subdir.
	assert.FileExists(t, filepath.Join(root, ".kata.toml"))
	assert.NoFileExists(t, filepath.Join(sub, ".kata.toml"))

	// .gitignore must follow .kata.toml — at the git root.
	rootIgnore := filepath.Join(root, ".gitignore")
	assert.FileExists(t, rootIgnore)
	content, err := os.ReadFile(rootIgnore) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), ".kata.local.toml")

	// And nothing was written in the subdir.
	assert.NoFileExists(t, filepath.Join(sub, ".gitignore"))
}

// fakeDaemon is a minimal stub of POST /api/v1/projects that records the
// last request body so tests can assert what the CLI actually sent. It
// returns a synthetic project response without ever touching the
// filesystem, mirroring how a daemon on another host would respond.
type fakeDaemon struct {
	mu      sync.Mutex
	lastReq map[string]any
	srv     *httptest.Server
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects", func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(bs, &body)
		f.mu.Lock()
		f.lastReq = body
		f.mu.Unlock()
		name, _ := body["name"].(string)
		if name == "" {
			http.Error(w, `{"error":{"code":"validation","message":"name required"}}`, http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"project": map[string]any{
				"name": name,
			},
			"created": true,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaemon) request() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

// TestInit_RemoteClient_SendsNameNotPath verifies the CLI derives
// the project name locally and omits start_path from the request body when it
// can — that's the contract that lets a daemon on another host serve
// `kata init` without filesystem access to the client workspace.
func TestInit_RemoteClient_SendsNameNotPath(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	daemonStub := newFakeDaemon(t)

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{})
	require.NoError(t, err)

	req := daemonStub.request()
	require.NotNil(t, req)
	assert.Equal(t, "kata", req["name"])
	assert.NotContains(t, req, "project_identity")
	assert.NotContains(t, req, "start_path", "remote init must not leak client filesystem path")

	// Client wrote .kata.toml itself — daemon never had FS access.
	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
}

// TestInit_RemoteClient_WritesGitignore confirms the .gitignore entry
// still lands beside .kata.toml in the client workspace, even though
// the daemon doesn't return workspace_root in path-free mode.
func TestInit_RemoteClient_WritesGitignore(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	daemonStub := newFakeDaemon(t)

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, ".gitignore")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), ".kata.local.toml")
}

// TestInit_RemoteClient_FromSubdir runs init from a subdirectory of
// the git workspace. .kata.toml must land at the git root, not the
// subdir, even though the daemon can't see either path.
func TestInit_RemoteClient_FromSubdir(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "--quiet")
	runGit(t, root, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	sub := filepath.Join(root, "internal", "tui")
	require.NoError(t, os.MkdirAll(sub, 0o755)) //nolint:gosec // test fixture under TempDir

	daemonStub := newFakeDaemon(t)

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), daemonStub.srv.URL, sub, callInitOpts{})
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(root, ".kata.toml"))
	assert.NoFileExists(t, filepath.Join(sub, ".kata.toml"))
	assert.FileExists(t, filepath.Join(root, ".gitignore"))
	assert.NoFileExists(t, filepath.Join(sub, ".gitignore"))
}

// TestInit_RemoteClient_ConflictDetectedLocally asserts that a
// client-side .kata.toml conflict with --project (without --replace)
// fails before any daemon round-trip, so a remote daemon never sees a
// stale name. The error must also carry the structured
// "project_binding_conflict" code so --json consumers can branch on
// it (matching the daemon-side conflict envelope).
func TestInit_RemoteClient_ConflictDetectedLocally(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), //nolint:gosec // test fixture matches production .kata.toml mode
		[]byte(`version = 1

[project]
identity = "github.com/wesm/kata"
name     = "kata"
`), 0o644))

	daemonStub := newFakeDaemon(t)

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), daemonStub.srv.URL, dir,
		callInitOpts{Project: "other"})
	require.Error(t, err)
	var ce *cliError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, ExitConflict, ce.ExitCode)
	assert.Equal(t, "project_binding_conflict", ce.Code,
		"--json consumers branch on error.code; client-side conflict must match daemon shape")

	// The daemon must not have been called — conflict was caught client-side.
	assert.Nil(t, daemonStub.request())
}

// TestInit_RemoteClient_SendsAliasInfo verifies the CLI computes alias
// metadata locally and includes it in the request body. The daemon
// uses that metadata to attach the alias on its side, so the
// alias-conflict and --reassign semantics survive the path-free flow.
func TestInit_RemoteClient_SendsAliasInfo(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	daemonStub := newFakeDaemon(t)

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{})
	require.NoError(t, err)

	req := daemonStub.request()
	require.NotNil(t, req)
	alias, ok := req["alias"].(map[string]any)
	require.True(t, ok, "request must include alias metadata when client has a git workspace; got: %v", req)
	assert.Equal(t, "github.com/wesm/kata", alias["identity"])
	assert.Equal(t, "git", alias["kind"])
	assert.Equal(t, dir, alias["root_path"])
}

func TestInit_GitignoreAppendsToExisting(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), //nolint:gosec // test fixture mirrors production .gitignore mode
		[]byte("dist/\n"), 0o644))

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, ".gitignore")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), "dist/")
	assert.Contains(t, string(content), ".kata.local.toml")
}

func TestInit_AgentOutputReportsProjectWorkspaceAndChanged(t *testing.T) {
	resetFlags(t)
	flags.Agent = true
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	daemonStub := newFakeDaemon(t)

	out, err := callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{})

	require.NoError(t, err)
	assert.Equal(t, "OK init project=kata workspace="+agentValue(dir)+" changed=true\n", out)
}

func TestInit_AgentOutputChangedWhenExistingProjectBindsFreshWorkspace(t *testing.T) {
	resetFlags(t)
	flags.Agent = true
	env := testenv.New(t)
	_, err := env.DB.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	out, err := callInit(context.Background(), env.URL, dir, callInitOpts{})

	require.NoError(t, err)
	assert.Equal(t, "OK init project=kata workspace="+agentValue(dir)+" changed=true\n", out)
}

func TestInit_HumanOutputKeepsProjectCreatedSemanticsForFreshWorkspace(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	_, err := env.DB.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	out, err := callInit(context.Background(), env.URL, dir, callInitOpts{})

	require.NoError(t, err)
	assert.Equal(t, "bound project kata\n", out)
}

func TestInit_AgentOutputChangedWhenOnlyGitignoreUpdated(t *testing.T) {
	resetFlags(t)
	flags.Agent = true
	env := testenv.New(t)
	_, err := env.DB.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), //nolint:gosec // test fixture matches production .kata.toml mode
		[]byte(`version = 1

[project]
identity = "github.com/wesm/kata"
name     = "kata"
`), 0o644))

	out, err := callInit(context.Background(), env.URL, dir, callInitOpts{})

	require.NoError(t, err)
	assert.Equal(t, "OK init project=kata workspace="+agentValue(dir)+" changed=true\n", out)
}

func TestInit_AgentOutputQuotesProjectName(t *testing.T) {
	resetFlags(t)
	flags.Agent = true
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	daemonStub := newFakeDaemon(t)

	out, err := callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{Project: "two words"})

	require.NoError(t, err)
	assert.Equal(t, "OK init project=\"two words\" workspace="+agentValue(dir)+" changed=true\n", out)
}

func TestInit_MachineOutputSuppressesGitignoreWarning(t *testing.T) {
	tests := []struct {
		name      string
		configure func()
		wantOut   string
	}{
		{
			name: "agent",
			configure: func() {
				flags.Agent = true
			},
			wantOut: "OK init project=kata workspace=",
		},
		{
			name: "json",
			configure: func() {
				flags.JSON = true
			},
			wantOut: `"kata_api_version":1`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
			require.NoError(t, os.Mkdir(filepath.Join(dir, ".gitignore"), 0o755)) //nolint:gosec // test fixture under TempDir
			daemonStub := newFakeDaemon(t)
			tt.configure()

			var out string
			var err error
			stderr := captureProcessStderr(t, func() {
				out, err = callInit(context.Background(), daemonStub.srv.URL, dir, callInitOpts{})
			})

			require.NoError(t, err)
			assert.Contains(t, out, tt.wantOut)
			assert.Empty(t, stderr)
		})
	}
}

func captureProcessStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })

	fn()

	require.NoError(t, w.Close())
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	os.Stderr = old
	return buf.String()
}

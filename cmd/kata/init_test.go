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

func TestInit_WithAgents_WritesAgentsFileWhenAbsent(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), agentsBlockBegin)
	assert.Contains(t, string(content), agentsBlockEnd)
	assert.Contains(t, string(content), "kata quickstart")
}

func TestInit_WithoutFlag_DoesNotWriteAgents(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{})
	require.NoError(t, err)

	assert.NoFileExists(t, filepath.Join(dir, "AGENTS.md"))
}

func TestInit_WithAgents_Idempotent(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)
	_, err = callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	// Exactly one managed block — no duplication on re-run.
	assert.Equal(t, 1, strings.Count(string(content), agentsBlockBegin))
	assert.Equal(t, 1, strings.Count(string(content), agentsBlockEnd))
}

func TestInit_WithAgents_AppendsToExistingFile(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), //nolint:gosec // test fixture under TempDir
		[]byte("# House rules\n\nRun the linter before committing.\n"), 0o644))

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	// Pre-existing content is preserved and the managed block is appended.
	assert.Contains(t, string(content), "Run the linter before committing.")
	assert.Contains(t, string(content), agentsBlockBegin)
}

func TestInit_WithAgents_RefreshesStaleBlock(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")
	stale := agentsBlockBegin + "\nold kata guidance that should be replaced\n" + agentsBlockEnd + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), //nolint:gosec // test fixture under TempDir
		[]byte("# House rules\n\n"+stale), 0o644))

	flags.JSON = true
	t.Cleanup(func() { flags.JSON = false })

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.NotContains(t, string(content), "old kata guidance that should be replaced")
	assert.Contains(t, string(content), "kata quickstart")
	assert.Contains(t, string(content), "# House rules", "content outside the markers must survive")
	assert.Equal(t, 1, strings.Count(string(content), agentsBlockBegin))
}

// beadsFixtureBlock is a stand-in for the integration block Beads writes into
// AGENTS.md/CLAUDE.md. kata matches on the begin-marker prefix and the end
// marker, so the trailing version/profile/hash here is representative.
const beadsFixtureBlock = "<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:deadbeef -->\n" +
	"## Beads issue tracker\n\nRun `bd quickstart`.\n" +
	"<!-- END BEADS INTEGRATION -->\n"

// When AGENTS.md still carries a beads block, kata must not edit it in place.
// It leaves the original byte-for-byte and writes a .kata-proposed sidecar with
// the beads block removed and kata's block added, and warns where to find it.
func TestInit_WithAgents_BeadsBlockInAgents_WritesSidecar(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	original := "# Agent guidance\n\nLegacy notes.\n\n" + beadsFixtureBlock
	agentsPath := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsPath, []byte(original), 0o644)) //nolint:gosec // test fixture under TempDir

	var callErr error
	stderr := captureProcessStderr(t, func() {
		_, callErr = callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	})
	require.NoError(t, callErr)

	// Original is untouched.
	got, err := os.ReadFile(agentsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, original, string(got), "AGENTS.md with a beads block must be left untouched")

	// Sidecar carries kata's block, drops the beads block, keeps other content.
	sidecar := agentsPath + agentsProposalSuffix
	proposed, err := os.ReadFile(sidecar) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(proposed), "Legacy notes.")
	assert.Contains(t, string(proposed), agentsBlockBegin)
	assert.Contains(t, string(proposed), "kata quickstart")
	assert.NotContains(t, string(proposed), beadsBlockBeginPrefix)
	assert.NotContains(t, string(proposed), beadsBlockEnd)

	// The user is told what to do.
	assert.Contains(t, stderr, "beads integration block")
	assert.Contains(t, stderr, filepath.Base(sidecar))
}

// The adopt hint must name paths that resolve from the caller's working
// directory. Run from the workspace root, paths collapse to clean base names;
// run from a subdirectory (or with --workspace pointing elsewhere), a bare base
// name would target the wrong directory, so the hint spells out absolute paths.
func TestBeadsConflictMessage_PathsResolveFromCwd(t *testing.T) {
	sidecar := "/repo/AGENTS.md" + agentsProposalSuffix

	fromRoot := beadsConflictMessage("/repo", "/repo/AGENTS.md", sidecar)
	assert.Contains(t, fromRoot, "AGENTS.md still contains a beads integration block")
	assert.Contains(t, fromRoot, "mv AGENTS.md"+agentsProposalSuffix+" AGENTS.md",
		"from the workspace root the mv command uses base names")

	fromSubdir := beadsConflictMessage("/repo/cmd/kata", "/repo/AGENTS.md", sidecar)
	assert.Contains(t, fromSubdir, "mv "+sidecar+" /repo/AGENTS.md",
		"outside the workspace root the mv command must use resolvable absolute paths")
	assert.NotContains(t, fromSubdir, "mv AGENTS.md"+agentsProposalSuffix+" AGENTS.md")
}

// A workspace path with spaces would, unquoted, parse as extra mv operands, so
// the adopt command must shell-quote both paths to stay copy-pasteable.
func TestBeadsConflictMessage_ShellQuotesMvOperands(t *testing.T) {
	original := "/Users/me/My Projects/app/AGENTS.md"
	sidecar := original + agentsProposalSuffix

	msg := beadsConflictMessage("/elsewhere", original, sidecar)
	assert.Contains(t, msg, "mv '"+sidecar+"' '"+original+"'",
		"mv operands must be single-quoted when the path contains spaces")
}

// A real (non-symlink) CLAUDE.md that still carries a beads block gets the same
// sidecar treatment as AGENTS.md.
func TestInit_WithAgents_BeadsBlockInClaude_WritesSidecar(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	claude := "# Claude guidance\n\n" + beadsFixtureBlock
	claudePath := filepath.Join(dir, "CLAUDE.md")
	require.NoError(t, os.WriteFile(claudePath, []byte(claude), 0o644)) //nolint:gosec // test fixture under TempDir

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	// CLAUDE.md itself is untouched.
	got, err := os.ReadFile(claudePath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, claude, string(got), "CLAUDE.md with a beads block must be left untouched")

	proposed, err := os.ReadFile(claudePath + agentsProposalSuffix) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(proposed), "# Claude guidance")
	assert.Contains(t, string(proposed), agentsBlockBegin)
	assert.NotContains(t, string(proposed), beadsBlockBeginPrefix)

	// AGENTS.md (absent here) still gets kata's block written normally.
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(agents), agentsBlockBegin)
}

// kata's own convention symlinks CLAUDE.md at AGENTS.md. A symlinked CLAUDE.md
// must never be rewritten or shadowed by a sidecar, even if it resolves to
// content with a beads block.
func TestInit_WithAgents_ClaudeSymlink_Skipped(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	// AGENTS.md holds a beads block; CLAUDE.md points at it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), //nolint:gosec // test fixture under TempDir
		[]byte("# Agent guidance\n\n"+beadsFixtureBlock), 0o644))
	require.NoError(t, os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")))

	_, err := callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})
	require.NoError(t, err)

	// No sidecar for the symlink, and the symlink itself is preserved.
	assert.NoFileExists(t, filepath.Join(dir, "CLAUDE.md"+agentsProposalSuffix))
	fi, err := os.Lstat(filepath.Join(dir, "CLAUDE.md"))
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&os.ModeSymlink, "CLAUDE.md must remain a symlink")
}

// A hostile repo must not redirect kata's sidecar write through a pre-planted
// symlink to clobber a file outside the workspace.
func TestInit_WithAgents_SidecarSymlinkNotFollowed(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	victim := filepath.Join(t.TempDir(), "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("precious\n"), 0o644)) //nolint:gosec // test fixture under TempDir

	agentsPath := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(agentsPath, //nolint:gosec // test fixture under TempDir
		[]byte("# Agent guidance\n\n"+beadsFixtureBlock), 0o644))
	// Attacker pre-plants the sidecar path as a symlink at the victim file.
	require.NoError(t, os.Symlink(victim, agentsPath+agentsProposalSuffix))

	_, _ = callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})

	got, err := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, "precious\n", string(got),
		"kata must not write through a pre-planted sidecar symlink")
}

// kata must not write through a symlinked AGENTS.md, which a hostile repo could
// point at a victim file to have init overwrite it.
func TestInit_WithAgents_AgentsSymlinkNotFollowed(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	victim := filepath.Join(t.TempDir(), "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("precious\n"), 0o644)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.Symlink(victim, filepath.Join(dir, "AGENTS.md")))

	_, _ = callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})

	got, err := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, "precious\n", string(got),
		"kata must not write through a symlinked AGENTS.md")
}

// A symlinked AGENTS.md whose target carries a beads block must not be migrated:
// following it would copy the outside file's content into AGENTS.md.kata-proposed
// inside the repo.
func TestInit_WithAgents_AgentsBeadsSymlinkNotMigrated(t *testing.T) {
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	// An outside file that happens to carry a beads block; a hostile repo points
	// AGENTS.md at it to coax kata into copying its content into the workspace.
	secret := filepath.Join(t.TempDir(), "secret.md")
	body := "# secrets\n\n" + beadsFixtureBlock
	require.NoError(t, os.WriteFile(secret, []byte(body), 0o644)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.Symlink(secret, filepath.Join(dir, "AGENTS.md")))

	_, _ = callInit(context.Background(), env.URL, dir, callInitOpts{WithAgents: true})

	// kata must not copy the outside file into a sidecar in the repo.
	assert.NoFileExists(t, filepath.Join(dir, "AGENTS.md"+agentsProposalSuffix))
	got, err := os.ReadFile(secret) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "the symlink target must be left untouched")
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

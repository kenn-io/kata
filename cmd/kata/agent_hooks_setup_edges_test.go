package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAgentHooksSetupEmptyDiscoveryAndSharedDirectories(t *testing.T) {
	isolateAgentHookHomes(t)
	dir := t.TempDir()
	for _, name := range []string{".github", ".agents"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	out, err := runHookSetup(t, "install", "--local", "--workspace", dir, "--executable", os.Args[0], "--json")
	require.ErrorContains(t, err, "no configured agents")
	require.Equal(t, ExitUsage, exitCodeForErr(err, true))
	require.Empty(t, out)
	out, err = runHookSetup(t, "install", "--all", "--local", "--workspace", dir, "--executable", os.Args[0], "--json")
	require.NoError(t, err)
	var report struct {
		Results []agentHookMutation `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, result := range report.Results {
		require.Equal(t, "skipped", result.State)
		require.False(t, result.Changed)
	}
	for _, name := range []string{".github", ".agents"} {
		entries, err := os.ReadDir(filepath.Join(dir, name))
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestAgentHooksSetupNativeComponentRemovalAndNoWriteReinstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, err := runHookSetup(t, "install", "pi", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, ".pi", "agent", "extensions", "kata.js")
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated generated artifact.
	require.NoError(t, err)
	meta, err := parsePiAgentHookMetadata(before)
	require.NoError(t, err)
	require.True(t, meta.Contract)
	require.True(t, meta.Attention)
	stamp := time.Unix(1000000000, 0)
	require.NoError(t, os.Chtimes(path, stamp, stamp))
	for _, selection := range []string{"--contract-only", "--attention=false", "--attention"} {
		_, err := runHookSetup(t, "install", "pi", "--executable", os.Args[0], selection)
		require.NoError(t, err)
		after, err := os.ReadFile(path) //nolint:gosec // G304: isolated generated artifact.
		require.NoError(t, err)
		require.Equal(t, before, after)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, stamp, info.ModTime())
	}
	_, err = runHookSetup(t, "uninstall", "pi", "--attention=false")
	require.NoError(t, err)
	data, err := os.ReadFile(path) //nolint:gosec // G304: isolated generated artifact.
	require.NoError(t, err)
	meta, err = parsePiAgentHookMetadata(data)
	require.NoError(t, err)
	require.False(t, meta.Contract)
	require.True(t, meta.Attention)
	_, err = runHookSetup(t, "uninstall", "pi")
	require.NoError(t, err)
	require.NoFileExists(t, path)
}

func TestAgentHooksRuntimeForeignArtifactIsNotAutomaticProbeSkip(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, marker := installOpenCodeVersionFixture(t, "exit 3")
	path := filepath.Join(home, ".config", "opencode", "plugin", "kata-user.js")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	foreign := []byte("export default function authoredPlugin() {}\n")
	require.NoError(t, os.WriteFile(path, foreign, 0600))
	require.NoError(t, os.Mkdir(filepath.Join(home, "codex"), 0700))
	_, err := runHookSetup(t, "install", "--executable", os.Args[0])
	require.ErrorContains(t, err, "preserving")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated authored plugin fixture.
	require.NoError(t, err)
	require.Equal(t, foreign, after)
	require.NoFileExists(t, filepath.Join(home, "codex", "hooks.json"))
	require.NoFileExists(t, marker)
}

func TestInitAgentHooksResultInMachineOutput(t *testing.T) {
	for _, mode := range []string{"json", "agent"} {
		for _, target := range []string{"muse", "codex"} {
			t.Run(mode+"/"+target, func(t *testing.T) {
				home := isolateAgentHookHomes(t)
				dir := t.TempDir()
				daemon := newFakeDaemon(t)
				resetFlags(t)
				root := newRootCmd()
				var output, diagnostic bytes.Buffer
				root.SetOut(&output)
				root.SetErr(&diagnostic)
				root.SetIn(unreadableHookInput{})
				root.SetArgs([]string{"init", "--agent-hooks=" + target, "--workspace", dir, "--project", "example-project", "--as", "example-actor", "--" + mode})
				require.NoError(t, root.ExecuteContext(contextWithBaseURL(t.Context(), daemon.srv.URL)))
				require.Empty(t, diagnostic.String())
				state := "installed"
				if target == "muse" {
					state = "partial"
				}
				if mode == "json" {
					var result struct {
						AgentHooks []agentHookMutation `json:"agent_hooks"`
					}
					require.NoError(t, json.Unmarshal(output.Bytes(), &result))
					require.Len(t, result.AgentHooks, 1)
					require.Equal(t, target, result.AgentHooks[0].Harness)
					require.Equal(t, state, result.AgentHooks[0].State)
					require.FileExists(t, result.AgentHooks[0].ConfigPath)
					if target == "muse" {
						require.Contains(t, result.AgentHooks[0].Reason, "Muse")
					} else {
						require.Empty(t, result.AgentHooks[0].Reason)
					}
				} else {
					require.Contains(t, output.String(), "agent_hooks_state="+state)
				}
				require.NoDirExists(t, filepath.Join(home, ".config", "muse"))
			})
		}
	}
}

func TestAgentHooksStatusInspectionErrorNamesScope(t *testing.T) {
	isolateAgentHookHomes(t)
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".muse"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".muse", "hooks.json"), []byte("{invalid"), 0600))
	out, err := runHookSetup(t, "status", "muse", "--local", "--workspace", dir)
	require.NoError(t, err)
	require.Contains(t, out, "muse (project): inspection unavailable")
}

func TestAgentHooksRuntimeTypeScriptConflictPrecedesProbeSkip(t *testing.T) {
	for _, version := range []string{"1.0.154", "2.0.0"} {
		for _, probe := range []string{"failed", "missing"} {
			t.Run(version+"/"+probe, func(t *testing.T) {
				home := isolateAgentHookHomes(t)
				installOpenCodeVersionFixture(t, `printf '%s\n' "$KATA_HOOK_TEST_VERSION"`)
				t.Setenv("KATA_HOOK_TEST_VERSION", version)
				out, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0], "--json")
				require.NoError(t, err)
				var installed struct {
					Results []agentHookMutation `json:"results"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &installed))
				require.Len(t, installed.Results, 1)
				path := installed.Results[0].ConfigPath
				owned, err := os.ReadFile(path) //nolint:gosec // G304: isolated owned plugin fixture.
				require.NoError(t, err)
				sibling := strings.TrimSuffix(path, ".js") + ".ts"
				authored := []byte("export default function authoredPlugin() {}\n")
				require.NoError(t, os.WriteFile(sibling, authored, 0600))
				require.NoError(t, os.Mkdir(filepath.Join(home, "codex"), 0700))
				if probe == "failed" {
					installOpenCodeVersionFixture(t, "exit 3")
				} else {
					t.Setenv("PATH", t.TempDir())
				}
				args := []string{"install", "--executable", os.Args[0], "--json"}
				if probe == "missing" {
					args = append(args, "--all")
				}
				_, err = runHookSetup(t, args...)
				require.ErrorContains(t, err, "preserving authored TypeScript plugin")
				require.NoFileExists(t, filepath.Join(home, "codex", "hooks.json"))
				after, err := os.ReadFile(path) //nolint:gosec // G304: isolated owned plugin fixture.
				require.NoError(t, err)
				require.Equal(t, owned, after)
				after, err = os.ReadFile(sibling) //nolint:gosec // G304: isolated authored plugin fixture.
				require.NoError(t, err)
				require.Equal(t, authored, after)
				_, err = runHookSetup(t, "status", "opencode", "--json")
				require.NoError(t, err)
				_, err = runHookSetup(t, "uninstall", "opencode")
				require.NoError(t, err)
				require.NoFileExists(t, path)
				require.FileExists(t, sibling)
			})
		}
	}
}

func TestAgentHooksRuntimeFreshTypeScriptConflictPrecedesProbeSkip(t *testing.T) {
	for _, api := range []string{"v1", "v2"} {
		for _, scope := range []string{"user", "project"} {
			for _, probe := range []string{"failed", "missing"} {
				t.Run(api+"/"+scope+"/"+probe, func(t *testing.T) {
					home := isolateAgentHookHomes(t)
					dir := t.TempDir()
					root := filepath.Join(home, ".config", "opencode")
					codexPath := filepath.Join(home, "codex", "hooks.json")
					require.NoError(t, os.Mkdir(filepath.Dir(codexPath), 0700))
					if scope == "project" {
						root = filepath.Join(dir, ".opencode")
						codexPath = filepath.Join(dir, ".codex", "hooks.json")
					}
					authoredPath := filepath.Join(root, "plugin", "kata-"+scope+".ts")
					if api == "v2" {
						authoredPath = filepath.Join(root, "plugins", "kata-"+scope, "index.ts")
					}
					require.NoError(t, os.MkdirAll(filepath.Dir(authoredPath), 0700))
					authored := []byte("export default function authoredPlugin() {}\n")
					require.NoError(t, os.WriteFile(authoredPath, authored, 0600))
					marker := ""
					if probe == "failed" {
						_, marker = installOpenCodeVersionFixture(t, "exit 3")
					} else {
						t.Setenv("PATH", t.TempDir())
					}
					args := []string{"install", "--scope", scope, "--workspace", dir, "--executable", os.Args[0], "--json"}
					if probe == "missing" {
						args = append(args, "--all")
					}
					_, err := runHookSetup(t, args...)
					require.ErrorContains(t, err, "preserving authored TypeScript plugin")
					require.NoFileExists(t, codexPath)
					require.NoFileExists(t, strings.TrimSuffix(authoredPath, ".ts")+".js")
					after, err := os.ReadFile(authoredPath) //nolint:gosec // G304: isolated authored plugin fixture.
					require.NoError(t, err)
					require.Equal(t, authored, after)
					if marker != "" {
						require.NoFileExists(t, marker, "collision must precede the probe")
					}
				})
			}
		}
	}
}

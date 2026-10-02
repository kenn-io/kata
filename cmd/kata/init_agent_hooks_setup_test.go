package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kit/agenthook"
)

func TestInitAgentHooksPublicationFailureReportsProjectAndBinding(t *testing.T) {
	for _, mode := range []outputMode{outputJSON, outputAgent, outputHuman} {
		t.Run(string(mode), func(t *testing.T) {
			isolateAgentHookHomes(t)
			env := testenv.New(t)
			dir := t.TempDir()
			path := filepath.Join(dir, ".codex", "hooks.json")
			lockPath, err := nativeAgentHookLockPath(path)
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(lockPath), 0700))
			lock := flock.New(lockPath)
			require.NoError(t, lock.Lock())
			t.Cleanup(func() { require.NoError(t, lock.Unlock()) })
			for _, created := range []bool{true, false} {
				args := []string{"init", "--agent-hooks=codex", "--project", "example-project", "--as", "example-actor"}
				if mode != outputHuman {
					args = append(args, "--"+string(mode))
				}
				out, err := runCLICapture(t, env, dir, args...)
				require.Error(t, err)
				require.Empty(t, out)
				project, dbErr := env.DB.ProjectByName(t.Context(), "example-project")
				require.NoError(t, dbErr)
				binding, bindingErr := config.ReadProjectConfig(dir)
				require.NoError(t, bindingErr)
				require.Equal(t, project.Name, binding.Project.Name)
				requireCodexComponents(t, path, false, false, false)
				require.Equal(t, ExitInternal, exitCodeForErr(err, true))
				var cli *cliError
				require.ErrorAs(t, err, &cli)
				require.Equal(t, "init_agent_hooks_failed", cli.Code)
				var data struct {
					Project struct {
						Name string `json:"name"`
					} `json:"project"`
					Created       bool   `json:"created"`
					Bound         bool   `json:"bound"`
					WorkspaceRoot string `json:"workspace_root"`
					HookError     string `json:"agent_hooks_error"`
					HooksChanged  bool   `json:"agent_hooks_changed"`
				}
				require.NoError(t, json.Unmarshal(cli.Data, &data))
				require.Equal(t, project.Name, data.Project.Name)
				require.Equal(t, created, data.Created)
				require.True(t, data.Bound)
				require.Equal(t, dir, data.WorkspaceRoot)
				require.Contains(t, data.HookError, "is being updated")
				require.False(t, data.HooksChanged)
				var rendered bytes.Buffer
				emitErrorForMode(&rendered, err, mode, true)
				require.Contains(t, rendered.String(), "bound project example-project")
				if created {
					require.Contains(t, rendered.String(), "created and bound")
				}
				if mode == outputJSON {
					var envelope struct {
						Error struct {
							Data json.RawMessage `json:"data"`
						} `json:"error"`
					}
					require.NoError(t, json.Unmarshal(rendered.Bytes(), &envelope))
					require.JSONEq(t, string(cli.Data), string(envelope.Error.Data))
				}
			}
		})
	}
}

func TestInitAgentHooksCodexRespectsUserContractAndTrackedWorkspace(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, contractOnly := range []bool{false, true} {
			t.Run(map[bool]string{false: "untracked", true: "tracked"}[tracked]+"/"+map[bool]string{false: "bundle", true: "contract"}[contractOnly], func(t *testing.T) {
				home := isolateAgentHookHomes(t)
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
				dir := t.TempDir()
				runGit(t, dir, "init", "--quiet")
				path := filepath.Join(dir, ".codex", "hooks.json")
				// A pre-existing workspace contract must be removed only when local.
				writeCodexFixture(t, path, `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-contract-hook","commandWindows":"kata agent-contract-hook"}]}]}}`)
				if tracked {
					runGit(t, dir, "add", ".codex/hooks.json")
				}
				_, err := runHookSetup(t, "install", "codex", "--contract-only", "--executable", "kata")
				require.NoError(t, err)
				userPath, err := agenthook.ConfigPath(agenthook.AgentCodex)
				require.NoError(t, err)
				userBefore, err := os.ReadFile(userPath) //nolint:gosec // Isolated user hook fixture.
				require.NoError(t, err)
				args := []string{"--agent-hooks=codex", "--workspace", dir, "--project", "example-project", "--as", "example-actor"}
				if contractOnly {
					args = append(args, "--contract-only")
				}
				daemon := newFakeDaemon(t)
				require.NoError(t, executeInitSetup(t, daemon.srv.URL, args...))
				requireCodexComponents(t, path, tracked, !contractOnly, !contractOnly)
				userAfter, err := os.ReadFile(userPath) //nolint:gosec // Isolated user hook fixture.
				require.NoError(t, err)
				require.Equal(t, userBefore, userAfter)
			})
		}
	}
}

func TestFormatInitOutputJSONHasStableHookFieldOrder(t *testing.T) {
	resetFlags(t)
	flags.Mode = outputJSON
	hooks := []agentHookMutation{{Harness: "codex", State: "installed", Warnings: []string{}}}
	response := []byte(`{"project":{"name":"example-project"},"created":true,"extra":42}`)
	for range 30 {
		output, err := formatInitOutput(response, "example-project", "", true, true, hooks)
		require.NoError(t, err)
		require.Less(t, strings.Index(output, `"agent_hooks":`), strings.Index(output, `"created":`))
		require.Less(t, strings.Index(output, `"created":`), strings.Index(output, `"extra":`))
		require.Less(t, strings.Index(output, `"extra":`), strings.Index(output, `"project":`))
	}
}

func TestFormatInitOutputJSONRejectsNonObjects(t *testing.T) {
	for _, response := range []string{"null", " \n\tnull\r\n ", "[]", `"unexpected"`, "42", "true"} {
		for _, withHooks := range []bool{false, true} {
			t.Run(response+"/"+map[bool]string{false: "legacy", true: "hooks"}[withHooks], func(t *testing.T) {
				resetFlags(t)
				flags.Mode = outputJSON
				var hooks []agentHookMutation
				if withHooks {
					hooks = []agentHookMutation{{Harness: "codex", State: "installed"}}
				}
				var output string
				var err error
				require.NotPanics(t, func() {
					output, err = formatInitOutput([]byte(response), "example-project", "", true, true, hooks)
				})
				require.Error(t, err)
				require.Empty(t, output)
			})
		}
	}
}

func TestFormatInitOutputJSONPreservesDaemonObject(t *testing.T) {
	for _, withHooks := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "hooks"}[withHooks], func(t *testing.T) {
			resetFlags(t)
			flags.Mode = outputJSON
			var hooks []agentHookMutation
			if withHooks {
				hooks = []agentHookMutation{{Harness: "codex", State: "installed", Changed: true, Warnings: []string{}}}
			}
			response := `{"project":{"name":"example-project"},"created":true,"extra":{"value":9007199254740993}}`
			output, err := formatInitOutput([]byte(response), "example-project", "", true, true, hooks)
			require.NoError(t, err)
			var result map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(output), &result))
			require.JSONEq(t, "1", string(result["kata_api_version"]))
			delete(result, "kata_api_version")
			if withHooks {
				var got []agentHookMutation
				require.NoError(t, json.Unmarshal(result["agent_hooks"], &got))
				require.Equal(t, hooks, got)
				delete(result, "agent_hooks")
			} else {
				require.NotContains(t, result, "agent_hooks")
			}
			preserved, err := json.Marshal(result)
			require.NoError(t, err)
			require.JSONEq(t, response, string(preserved))
		})
	}
}

func executeInitSetup(t *testing.T, baseURL string, args ...string) error {
	t.Helper()
	resetFlags(t)
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetIn(unreadableHookInput{})
	root.SetArgs(append([]string{"init"}, args...))
	return root.ExecuteContext(contextWithBaseURL(t.Context(), baseURL))
}

func TestInitAgentHooksCSVDefaultsAndWorkspaceRoot(t *testing.T) {
	for _, contractOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "bundle", true: "contract"}[contractOnly], func(t *testing.T) {
			isolateAgentHookHomes(t)
			dir := t.TempDir()
			runGit(t, dir, "init", "--quiet")
			sub := filepath.Join(dir, "nested")
			require.NoError(t, os.Mkdir(sub, 0700))
			daemon := newFakeDaemon(t)
			args := []string{"--agent-hooks=codex,pi,codex", "--workspace", sub, "--project", "example-project", "--as", "example-actor"}
			if contractOnly {
				args = append(args, "--contract-only")
			}
			require.NoError(t, executeInitSetup(t, daemon.srv.URL, args...))
			require.Equal(t, "example-project", daemon.request()["name"])
			require.NotContains(t, daemon.request(), "start_path")
			requireCodexComponents(t, filepath.Join(dir, ".codex", "hooks.json"), true, !contractOnly, !contractOnly)
			data, err := os.ReadFile(filepath.Join(dir, ".pi", "extensions", "kata.js")) //nolint:gosec // G304: isolated native init fixture.
			require.NoError(t, err)
			meta, err := parsePiAgentHookMetadata(data)
			require.NoError(t, err)
			require.Equal(t, "kata", meta.Executable)
			require.Equal(t, !contractOnly, meta.Attention)
			require.NoDirExists(t, filepath.Join(sub, ".codex"))
		})
	}
}

func TestInitAgentHooksCSVRejectsInvalidAndMixedBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"--agent-hooks="}, {"--agent-hooks=codex,,pi"}, {"--agent-hooks=unknown"}, {"--agent-hooks=hermes"}, {"--contract-only"}, {"--agent-hooks=codex", "--with-codex-hooks"}, {"--agent-hooks=pi", "--with-agent-hooks=codex"}, {"--contract-only", "--with-hooks"}, {"--contract-only", "--with-agent-hooks=codex"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			isolateAgentHookHomes(t)
			dir := t.TempDir()
			daemon := newFakeDaemon(t)
			err := executeInitSetup(t, daemon.srv.URL, append(args, "--workspace", dir, "--project", "example-project")...)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "unknown flag")
			require.Nil(t, daemon.request())
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestInitAgentHooksOpenCodePinsPreflightSelection(t *testing.T) {
	isolateAgentHookHomes(t)
	executable, marker := installOpenCodeVersionFixture(t, "printf '1.0.154\\n'")
	dir := t.TempDir()
	var request map[string]any
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		// A second version selection would now fail after the project mutation.
		if err := os.Remove(executable); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project":{"name":"example-project"},"created":true}`))
	}))
	t.Cleanup(daemon.Close)
	err := executeInitSetup(t, daemon.URL, "--agent-hooks=opencode", "--workspace", dir, "--project", "example-project", "--as", "example-actor")
	require.NoError(t, err)
	require.Equal(t, "example-project", request["name"])
	require.NotContains(t, request, "start_path")
	calls, err := os.ReadFile(marker) //nolint:gosec // G304: isolated version-probe recorder.
	require.NoError(t, err)
	require.Equal(t, "probe\n", string(calls))
	require.FileExists(t, filepath.Join(dir, ".opencode", "plugin", "kata-project.js"))
}

func TestInitLegacyAgentHooksOpenCodeDetectsAndPinsAPI(t *testing.T) {
	isolateAgentHookHomes(t)
	_, marker := installOpenCodeVersionFixture(t, `printf '%s\n' "$KATA_HOOK_TEST_VERSION"`)
	t.Setenv("KATA_HOOK_TEST_VERSION", "1.0.154")
	dir := t.TempDir()
	daemon := newFakeDaemon(t)
	require.NoError(t, executeInitSetup(t, daemon.srv.URL, "--with-agent-hooks", "opencode", "--workspace", dir, "--project", "example-project", "--as", "example-actor"))
	calls, err := os.ReadFile(marker) //nolint:gosec // G304: isolated runtime probe recorder.
	require.NoError(t, err)
	require.Equal(t, "probe\n", string(calls))
	path := filepath.Join(dir, ".opencode", "plugin", "kata-project.js")
	data, err := os.ReadFile(path) //nolint:gosec // G304: generated plugin stays under the isolated workspace.
	require.NoError(t, err)
	meta, err := parsePluginAgentHookMetadata(data)
	require.NoError(t, err)
	require.Equal(t, "v1", meta.API)
	require.NoFileExists(t, filepath.Join(dir, ".opencode", "plugins", "kata-project", "index.js"))
}

func TestInitAgentHooksMuseNeverExpandsUserPolicy(t *testing.T) {
	home := isolateAgentHookHomes(t)
	dir := t.TempDir()
	daemon := newFakeDaemon(t)
	require.NoError(t, executeInitSetup(t, daemon.srv.URL, "--agent-hooks=muse", "--workspace", dir, "--project", "example-project", "--as", "example-actor"))
	require.FileExists(t, filepath.Join(dir, ".muse", "hooks.json"))
	require.NoDirExists(t, filepath.Join(home, ".config", "muse"))
	plan, err := planMuseAgentHooks(nativeAgentHookOptions{Agent: "muse", Scope: "project", Home: home, Dir: dir}, true)
	require.NoError(t, err)
	require.True(t, plan.CurrentContract)
	require.False(t, plan.CurrentAttentionStart)
	require.False(t, plan.CurrentAttentionEnd)
}

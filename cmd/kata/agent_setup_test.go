package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentSetupInstallsClaudeHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	resetFlags(t)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"agent":"claude"`)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "attention-native")
	require.Contains(t, string(data), "contract")
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--agent")
	require.NoError(t, err)
	require.Contains(t, out, "agent=claude")
	require.Contains(t, out, "state=ok")
	again, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, again)
}

func TestAgentSetupCheckDoesNotInstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	resetFlags(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"foreign-contract-check"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "Kata hooks are not installed")
	require.Contains(t, out, "kata agent setup claude")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, string(after))
}

func TestAgentSetupCodexTrust(t *testing.T) {
	isolateAgentHookHomes(t)
	installCodexAgentFixture(t, "normal")
	t.Setenv("TEST_HOOK_TRUST", "untrusted")
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex")
	require.Error(t, err)
	require.Contains(t, out, "needs you")
	require.Contains(t, out, "/hooks")
	t.Setenv("TEST_HOOK_TRUST", "trusted")
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--check", "--agent")
	require.NoError(t, err)
	require.Contains(t, out, "state=ok")
}

func TestAgentSetupNoDetectedAgent(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "--check")
	require.ErrorContains(t, err, "no supported agent found; install Claude Code or Codex, or name an agent")
}

func TestAgentSetupPreservesGroupMetadata(t *testing.T) {
	for _, name := range []string{"claude", "codex"} {
		t.Run(name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			if name == "claude" {
				installClaudeAgentFixture(t)
			} else {
				installCodexAgentFixture(t, "normal")
				t.Setenv("TEST_HOOK_TRUST", "trusted")
			}
			_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name)
			require.NoError(t, err)
			file := "settings.json"
			if name == "codex" {
				file = "hooks.json"
			}
			path := filepath.Join(home, name, file)
			data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(data, &doc))
			group := doc["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
			group["description"] = "metadata"
			group["hooks"] = append(group["hooks"].([]any), map[string]any{"type": "command", "command": "foreign-hook", "custom": "preserved"})
			data, err = json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
			require.NoError(t, err, out)
			if name == "codex" {
				group["if"] = "condition"
				data, err = json.Marshal(doc)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0600))
				out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--check")
				require.Error(t, err)
				require.Contains(t, out, "outdated")
			}
		})
	}
}

func TestAgentSetupWarnsForUnstableExecutable(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Setenv("PATH", t.TempDir())
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
	require.Error(t, err)
	require.True(t, json.Valid([]byte(out)))
	require.Contains(t, out, "Agent executable is missing")
	require.Contains(t, stderr, "may not survive an upgrade")
	require.Contains(t, stderr, "PATH")
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.NotContains(t, stderr, "may not survive an upgrade")
}

func TestAgentSetupForeignMarker(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	hooks := doc["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	owned := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	hooks["SessionStart"] = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo " + owned}}})
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
}

func TestAgentSetupPreservesBareExecutable(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	kata := "kata"
	if runtime.GOOS == "windows" {
		kata += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("PATH"), kata), []byte("fixture"), 0700)) //nolint:gosec // Executable fixture in a temporary directory.
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"}]}]}}`), 0600))
	_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	data, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Contains(t, string(data), "kata agent-hook attention-native claude start")
	require.Contains(t, string(data), "kata agent-hook attention-native claude end")
}

func TestAgentSetupConflictingExecutables(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "claude", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	data := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract claude"},{"type":"command","command":"/usr/bin/kata agent-hook attention-native claude start"}]}]}}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.Error(t, err)
	require.Contains(t, out, "unknown")
	require.Contains(t, out, "agent-hook install claude --executable ")
	after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
	require.NoError(t, err)
	require.Equal(t, data, string(after))
	if runtime.GOOS == "windows" {
		return // The recovery command below is executed by a POSIX shell.
	}
	// Recovery must work when Kata runs by absolute path and is absent from PATH.
	bin := buildAgentSetupRecovery(t)
	var output []byte
	t.Setenv("PATH", t.TempDir())
	setup := exec.CommandContext(t.Context(), bin, "agent", "setup", "claude", "--check", "--json") //nolint:gosec // Test-built executable and fixed arguments.
	output, err = setup.Output()
	require.Error(t, err)
	var report struct {
		Rows []struct {
			NextStep string `json:"next_step"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(output, &report))
	require.Len(t, report.Rows, 1)
	repair := exec.CommandContext(t.Context(), "/bin/sh", "-c", report.Rows[0].NextStep) //nolint:gosec // Execute the CLI's suggested command in the isolated home.
	output, err = repair.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestAgentSetupCountsOnlyModifiedHooks(t *testing.T) {
	for _, tc := range []struct {
		trust, state string
		count        int
	}{{"untrusted", "untrusted", 3}, {"one-modified", "modified", 1}} {
		t.Run(tc.trust, func(t *testing.T) {
			isolateAgentHookHomes(t)
			installCodexAgentFixture(t, "normal")
			t.Setenv("TEST_HOOK_TRUST", tc.trust)
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "codex", "--json")
			require.Error(t, err)
			var report struct {
				Rows []struct {
					State     string `json:"state"`
					HookCount int    `json:"hook_count"`
				} `json:"rows"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			require.Len(t, report.Rows, 1)
			require.Equal(t, tc.state, report.Rows[0].State)
			require.Equal(t, tc.count, report.Rows[0].HookCount)
		})
	}
}

func TestAgentSetupBrokenAgentDoesNotBlockOthers(t *testing.T) {
	home := isolateAgentHookHomes(t)
	path := filepath.Join(home, "codex", "hooks.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "codex", "--json")
	require.Error(t, err)
	var report struct {
		Rows []struct {
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Rows, 2)
	require.Equal(t, "claude", report.Rows[0].Agent)
	require.Equal(t, "failed", report.Rows[1].State)
	require.FileExists(t, filepath.Join(home, "claude", "settings.json"))
}

func TestAgentSetupExplicitKataOnlyAgent(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(home, "gemini"))
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "antigravity", "--check", "--json")
	require.Error(t, err)
	require.Contains(t, out, `"state":"unknown"`)
	require.NoFileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "antigravity", "gemini", "claude", "--json")
	require.Error(t, err)
	var report struct {
		Rows []struct {
			Agent string `json:"agent"`
			State string `json:"state"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Rows, 3)
	require.Equal(t, "antigravity", report.Rows[0].Agent)
	require.Equal(t, "unknown", report.Rows[0].State, out)
	require.Equal(t, "gemini", report.Rows[1].Agent)
	require.Equal(t, "unknown", report.Rows[1].State, out)
	require.Equal(t, "claude", report.Rows[2].Agent)
	require.FileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	require.FileExists(t, filepath.Join(home, "gemini", ".gemini", "settings.json"))
}

func TestAgentSetupKataOnlyProfiles(t *testing.T) {
	for _, name := range []string{"antigravity", "amp", "openclaw", "kimi-code", "kimi", "grok", "zcode", "pi"} {
		t.Run(name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--json")
			require.Error(t, err)
			require.Contains(t, out, `"state":"unknown"`)
			path, err := agentHookScopePath(name, "user", home)
			require.NoError(t, err)
			_, err = os.Stat(path)
			require.NoError(t, err)
		})
	}
}

func TestAgentSetupPreservesSelectedAgentExecutable(t *testing.T) {
	for _, name := range []string{"antigravity", "amp", "openclaw", "kimi-code", "kimi", "grok", "zcode", "pi"} {
		for _, absolute := range []bool{false, true} {
			if absolute && name != "pi" && name != "antigravity" {
				continue
			}
			label := "bare"
			if absolute {
				label = "absolute"
			}
			t.Run(name+"/"+label, func(t *testing.T) {
				home := isolateAgentHookHomes(t)
				chosen := "kata"
				if absolute {
					chosen = filepath.Join(t.TempDir(), "kata")
					require.NoError(t, os.WriteFile(chosen, []byte("fixture"), 0700)) //nolint:gosec // G306: executable fixture in a temporary directory.
				}
				_, err := runHookSetup(t, "install", name, "--executable", chosen)
				require.NoError(t, err)
				path, err := agentHookScopePath(name, "user", home)
				require.NoError(t, err)
				if name == "openclaw" {
					path = filepath.Join(path, "index.js")
				}
				before, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
				require.NoError(t, err)
				out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", name, "--json")
				require.Error(t, err)
				require.Contains(t, out, `"state":"unknown"`)
				after, err := os.ReadFile(path) //nolint:gosec // Path is under the isolated test home.
				require.NoError(t, err)
				require.Equal(t, before, after)
			})
		}
	}
}

func TestAgentSetupFallbackCancellation(t *testing.T) {
	home := isolateAgentHookHomes(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := agentSetupFallback(ctx, "antigravity", false, agentSetupItem("antigravity", func(string) {}))
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
}

func installClaudeAgentFixture(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0700)) //nolint:gosec // Executable fixture in a temporary directory.
	t.Setenv("PATH", dir)
}

func TestAgentSetupConditionalClaude(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(disabled), func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			installClaudeAgentFixture(t)
			path := filepath.Join(home, "claude", "settings.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			_, err := runHookSetup(t, "install", "claude", "--executable", os.Args[0])
			require.NoError(t, err)
			data, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			var doc map[string]any
			require.NoError(t, json.Unmarshal(data, &doc))
			doc["disableAllHooks"] = disabled
			group := doc["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
			group["if"] = "condition"
			data, err = json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--json")
			if disabled {
				require.Error(t, err)
				require.Contains(t, out, `"state":"disabled"`)
			} else {
				require.NoError(t, err)
				require.Contains(t, out, `"state":"ok"`)
				require.Contains(t, out, `"hook_count":3`)
			}
			before, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Contains(t, string(before), `"if": "condition"`)
			out, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check", "--json")
			if disabled {
				require.Error(t, err)
				require.Contains(t, out, `"state":"disabled"`)
			} else {
				require.NoError(t, err)
				require.Contains(t, out, `"state":"ok"`)
			}
			after, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestAgentSetupMissingExecutable(t *testing.T) {
	for _, name := range []string{"claude", "pi"} {
		t.Run(name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			var bin string
			if name == "claude" && runtime.GOOS != "windows" {
				bin = buildAgentSetupRecovery(t)
			}
			installClaudeAgentFixture(t)
			exe := filepath.Join(t.TempDir(), "old-kata")
			require.NoError(t, os.WriteFile(exe, []byte("fixture"), 0700)) //nolint:gosec // Executable fixture.
			_, err := runHookSetup(t, "install", name, "--executable", exe)
			require.NoError(t, err)
			path, err := agentHookScopePath(name, "user", home)
			require.NoError(t, err)
			before, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.NoError(t, os.Remove(exe))
			for _, args := range [][]string{{}, {"--check"}} {
				out, _, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent", "setup", name, "--json"}, args...)...)
				require.Error(t, err)
				require.Contains(t, out, `"state":"unknown"`)
				require.Contains(t, out, exe)
				require.Contains(t, out, "agent-hook install "+name+" --executable")
				after, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			if bin != "" {
				setup := exec.CommandContext(t.Context(), bin, "agent", "setup", "claude", "--check", "--json") //nolint:gosec // Test-built executable.
				output, err := setup.Output()
				require.Error(t, err)
				var report struct {
					Rows []struct {
						NextStep string `json:"next_step"`
					} `json:"rows"`
				}
				require.NoError(t, json.Unmarshal(output, &report))
				require.Len(t, report.Rows, 1)
				require.Contains(t, report.Rows[0].NextStep, bin)
				repair := exec.CommandContext(t.Context(), "/bin/sh", "-c", report.Rows[0].NextStep) //nolint:gosec // Suggested command in isolated home.
				output, err = repair.CombinedOutput()
				require.NoError(t, err, string(output))
				output, err = exec.CommandContext(t.Context(), bin, "agent", "setup", "claude", "--check", "--json").CombinedOutput() //nolint:gosec // Test-built executable.
				require.NoError(t, err, string(output))
				require.Contains(t, string(output), `"state":"ok"`)
			}
		})
	}
}

func TestAgentSetupSettingsWithoutHooks(t *testing.T) {
	for _, tc := range []struct{ name, dir, data string }{{"muse", ".config/muse", `{"schema_version":1,"theme":"dark"}`}, {"droid", ".factory", `{"theme":"dark"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			path := filepath.Join(home, tc.dir, "settings.json")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0600))
			check, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", tc.name, "--check", "--json")
			require.Error(t, err)
			require.Contains(t, check, `"state":"missing"`)
			before, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Equal(t, tc.data, string(before))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", tc.name, "--json")
			require.Error(t, err)
			require.Contains(t, out, `"state":"unknown"`)
			if tc.name == "muse" {
				require.Contains(t, out, "permission")
			}
			after, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Contains(t, string(after), `"dark"`)
			require.Contains(t, string(after), "agent-hook contract "+tc.name)
		})
	}
}

func TestAgentSetupOpenCodeMissingEntry(t *testing.T) {
	for _, absolute := range []bool{false, true} {
		t.Run(strconv.FormatBool(absolute), func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			installOpenCodeVersionFixture(t, `printf '2.0.0\n'`)
			chosen := "kata"
			if absolute {
				chosen = filepath.Join(t.TempDir(), "kata with spaces")
				require.NoError(t, os.WriteFile(chosen, []byte("fixture"), 0700)) //nolint:gosec // Executable fixture.
			}
			_, err := runHookSetup(t, "install", "opencode", "--api", "v2", "--executable", chosen)
			require.NoError(t, err)
			root := filepath.Join(home, ".config", "opencode", "plugins", "kata-user")
			js, manifest := filepath.Join(root, "index.js"), filepath.Join(root, "package.json")
			before, err := os.ReadFile(manifest) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.NoError(t, os.Remove(js))
			out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "opencode", "--json")
			require.Error(t, err)
			require.Contains(t, out, `"state":"unknown"`)
			code, err := os.ReadFile(js) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Contains(t, string(code), strconv.Quote(chosen))
			require.Contains(t, string(before), strconv.Quote(chosen))
			after, err := os.ReadFile(manifest) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "opencode", "--json")
			require.Error(t, err)
			again, err := os.ReadFile(js) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Equal(t, code, again)
			last, err := os.ReadFile(manifest) //nolint:gosec // Isolated fixture.
			require.NoError(t, err)
			require.Equal(t, before, last)
		})
	}
}

func buildAgentSetupRecovery(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kata with spaces")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, ".") //nolint:gosec // Test-owned output and fixed package.
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	return bin
}

func TestAgentSetupConditionalClaudeMissingBareExecutable(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installClaudeAgentFixture(t)
	kata := filepath.Join(os.Getenv("PATH"), "kata")
	if runtime.GOOS == "windows" {
		kata += ".exe"
	}
	require.NoError(t, os.WriteFile(kata, []byte("fixture"), 0700)) //nolint:gosec // Executable fixture.
	_, err := runHookSetup(t, "install", "claude", "--executable", "kata")
	require.NoError(t, err)
	path := filepath.Join(home, "claude", "settings.json")
	data, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(data, &doc))
	group := doc["hooks"].(map[string]any)["SessionStart"].([]any)[0].(map[string]any)
	group["if"] = "condition"
	data, err = json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	_, _, err = executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude")
	require.NoError(t, err)
	require.NoError(t, os.Remove(kata)) //nolint:gosec // Isolated fixture path.
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "claude", "--check", "--json")
	require.Error(t, err)
	require.Contains(t, out, `"state":"unknown"`)
	require.Contains(t, out, "PATH")
}

func TestAgentSetupDroidStandaloneHooks(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, err := runHookSetup(t, "install", "droid", "--executable", "kata")
	require.NoError(t, err)
	path := filepath.Join(home, ".factory", "hooks.json")
	before, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
	require.NoError(t, err)
	out, _, err := executeAgentHook(t, unreadableHookInput{}, "agent", "setup", "droid", "--json")
	require.Error(t, err)
	require.Contains(t, out, `"state":"unknown"`)
	after, err := os.ReadFile(path) //nolint:gosec // Isolated fixture.
	require.NoError(t, err)
	require.Equal(t, before, after)
}

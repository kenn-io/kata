package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func installOpenCodeVersionFixture(t *testing.T, body string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("version-process fixture uses a Unix executable")
	}
	bin := t.TempDir()
	path := filepath.Join(bin, "opencode")
	marker := filepath.Join(bin, "calls")
	t.Setenv("KATA_HOOK_TEST_CALLS", marker)
	content := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = --version ] || exit 9\nprintf 'probe\\n' >> \"$KATA_HOOK_TEST_CALLS\"\n" + body + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0700)) //nolint:gosec // G306: executable fixture in TempDir.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path, marker
}

func TestAgentHooksRuntimeOpenCodeVersions(t *testing.T) {
	for _, tc := range []struct{ version, api string }{{"1.0.154", "v1"}, {"opencode 1.12.0", "v1"}, {"2.0.0", "v2"}, {"v2.4.1+build.1", "v2"}, {"1.0.153", ""}, {"0.9.0", ""}, {"3.0.0", ""}, {"2.0.0-beta.1", ""}, {"build 2.0.0 from source", ""}, {"2.0.0+build..1", ""}, {"2.0.0+.", ""}} {
		t.Run(tc.version, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			installOpenCodeVersionFixture(t, `printf '%s\n' "$KATA_HOOK_TEST_VERSION"`)
			t.Setenv("KATA_HOOK_TEST_VERSION", tc.version)
			_, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0])
			root := filepath.Join(home, ".config", "opencode")
			if tc.api == "" {
				require.Error(t, err)
				require.NoDirExists(t, root)
				return
			}
			require.NoError(t, err)
			path := filepath.Join(root, "plugin", "kata-user.js")
			if tc.api == "v2" {
				path = filepath.Join(root, "plugins", "kata-user", "index.js")
			}
			data, err := os.ReadFile(path) //nolint:gosec // G304: isolated generated plugin fixture.
			require.NoError(t, err)
			meta, err := parsePluginAgentHookMetadata(data)
			require.NoError(t, err)
			require.Equal(t, tc.api, meta.API)
			require.True(t, meta.Contract)
			require.True(t, meta.Attention)
		})
	}
}

func TestAgentHooksRuntimeOpenCodeFailureOverrideAndOffline(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, marker := installOpenCodeVersionFixture(t, "exit 3")
	_, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0])
	require.ErrorContains(t, err, "--api")
	require.NoDirExists(t, filepath.Join(home, ".config", "opencode"))
	_, err = runHookSetup(t, "install", "opencode", "--api", "v1", "--executable", os.Args[0])
	require.NoError(t, err)
	before, err := os.ReadFile(marker) //nolint:gosec // G304: probe recorder under TempDir.
	require.NoError(t, err)
	_, err = runHookSetup(t, "status", "opencode", "--json")
	require.NoError(t, err)
	_, err = runHookSetup(t, "uninstall", "opencode")
	require.NoError(t, err)
	after, err := os.ReadFile(marker) //nolint:gosec // G304: probe recorder under TempDir.
	require.NoError(t, err)
	require.Equal(t, before, after, "status and uninstall must not probe a runtime")
}

func TestAgentHooksRuntimeOpenCodeMismatchPreservesFiles(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installOpenCodeVersionFixture(t, `printf '%s\n' "$KATA_HOOK_TEST_VERSION"`)
	t.Setenv("KATA_HOOK_TEST_VERSION", "1.0.154")
	_, err := runHookSetup(t, "install", "opencode", "--api", "v1", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, ".config", "opencode", "plugin", "kata-user.js")
	before, err := os.ReadFile(path) //nolint:gosec // G304: owned generated plugin fixture.
	require.NoError(t, err)
	t.Setenv("KATA_HOOK_TEST_VERSION", "2.0.0")
	for _, override := range []string{"", "v1", "v2"} {
		args := []string{"install", "opencode", "--executable", os.Args[0]}
		if override != "" {
			args = append(args, "--api", override)
		}
		_, err = runHookSetup(t, args...)
		require.Error(t, err, "override %q", override)
		after, readErr := os.ReadFile(path) //nolint:gosec // G304: owned generated plugin fixture.
		require.NoError(t, readErr)
		require.Equal(t, before, after)
		require.NoDirExists(t, filepath.Join(home, ".config", "opencode", "plugins"))
	}
}

func TestAgentHooksRuntimeOpenCodeTimeoutAndAutomaticSkip(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installOpenCodeVersionFixture(t, "exec /bin/sleep 30")
	started := time.Now()
	_, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0])
	require.ErrorContains(t, err, "--api")
	require.Less(t, time.Since(started), 15*time.Second)
	require.NoDirExists(t, filepath.Join(home, ".config", "opencode"))
	installOpenCodeVersionFixture(t, "exit 3")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "codex"), 0700))
	out, err := runHookSetup(t, "install", "--executable", os.Args[0], "--json")
	require.NoError(t, err)
	var report struct {
		Results []agentHookMutation `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, row := range report.Results {
		if row.Harness == "opencode" {
			require.Equal(t, "skipped", row.State)
			require.Contains(t, row.Reason, "--api")
		}
	}
	requireCodexComponents(t, filepath.Join(home, "codex", "hooks.json"), true, true, true)
}

func runMuseSetup(t *testing.T, terminal bool, input io.Reader, args ...string) (string, string, error) {
	t.Helper()
	resetFlags(t)
	root := newRootCmd()
	group, _, err := root.Find([]string{"agent-hook"})
	require.NoError(t, err)
	root.RemoveCommand(group)
	root.AddCommand(newAgentHooksCmdWithTerminalCheck(func(io.Reader) bool { return terminal }))
	return executeAgentHookRoot(t, root, input, append([]string{"agent-hook", "install"}, args...)...)
}

func TestAgentHooksMuseSetupConsent(t *testing.T) {
	for _, answer := range []string{"yes\n", "no\n", ""} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			out, diagnostic, err := runMuseSetup(t, true, strings.NewReader(answer), "muse", "--executable", os.Args[0])
			require.NoError(t, err)
			require.Contains(t, diagnostic, "forward")
			require.Contains(t, diagnostic, "all managed hooks")
			require.Contains(t, diagnostic, "KATA_AUTH_TOKEN")
			plan, err := planMuseAgentHooks(nativeAgentHookOptions{Agent: "muse", Scope: "user", Home: home}, true)
			require.NoError(t, err)
			require.True(t, plan.CurrentContract)
			approved := answer == "yes\n"
			require.Equal(t, approved, plan.CurrentAttentionStart)
			require.Equal(t, approved, plan.CurrentAttentionEnd)
			if !approved {
				require.Contains(t, out, "partial")
				require.Contains(t, out, "--managed-attention")
			}
		})
	}
}

func TestAgentHooksMuseSetupNoPromptOrImplicitPolicyExpansion(t *testing.T) {
	for _, mode := range []string{"noninteractive", "json", "agent", "automatic", "project"} {
		t.Run(mode, func(t *testing.T) {
			home := isolateAgentHookHomes(t)
			dir := t.TempDir()
			root := filepath.Join(home, ".config", "muse")
			require.NoError(t, os.MkdirAll(root, 0700))
			settings := []byte(`{"schema_version":1,"managed_hooks_path":"operator.json","managed_hooks_env_vars":["KATA_REF"]}`)
			require.NoError(t, os.WriteFile(filepath.Join(root, "settings.json"), settings, 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "operator.json"), []byte(`{}`), 0600))
			args := []string{"muse", "--executable", os.Args[0], "--workspace", dir}
			switch mode {
			case "json", "agent":
				args = append(args, "--"+mode)
			case "automatic":
				args = args[1:]
			case "project":
				args = append(args, "--local")
			}
			out, diagnostic, err := runMuseSetup(t, mode != "noninteractive", unreadableHookInput{}, args...)
			require.NoError(t, err)
			require.Empty(t, diagnostic)
			require.Contains(t, out, "partial")
			after, err := os.ReadFile(filepath.Join(root, "settings.json")) //nolint:gosec // G304: isolated Muse policy fixture.
			require.NoError(t, err)
			var policy map[string]any
			require.NoError(t, json.Unmarshal(after, &policy))
			require.Equal(t, "operator.json", policy["managed_hooks_path"])
			require.Equal(t, []any{"KATA_REF"}, policy["managed_hooks_env_vars"])
			if mode == "project" {
				require.Equal(t, settings, after)
			}
		})
	}
}

func TestAgentHooksMuseSetupReusesSufficientPolicyAndPreservesOnUninstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	_, _, err := runMuseSetup(t, false, unreadableHookInput{}, "muse", "--managed-attention", "--executable", os.Args[0])
	require.NoError(t, err)
	path := filepath.Join(home, ".config", "muse", "settings.json")
	before, err := os.ReadFile(path) //nolint:gosec // G304: isolated Muse policy fixture.
	require.NoError(t, err)
	_, err = runHookSetup(t, "uninstall", "muse")
	require.NoError(t, err)
	out, diagnostic, err := runMuseSetup(t, true, unreadableHookInput{}, "muse", "--executable", os.Args[0])
	require.NoError(t, err)
	require.Empty(t, diagnostic)
	require.NotContains(t, out, "partial")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated Muse policy fixture.
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
	_, _, err = runMuseSetup(t, false, unreadableHookInput{}, "muse", "--executable", os.Args[0])
	require.NoError(t, err)
	repeated, err := os.ReadFile(path) //nolint:gosec // G304: isolated Muse policy fixture.
	require.NoError(t, err)
	require.Equal(t, after, repeated)
	plan, err := planMuseAgentHooks(nativeAgentHookOptions{Agent: "muse", Scope: "user", Home: home}, true)
	require.NoError(t, err)
	require.True(t, plan.CurrentAttentionStart)
	require.True(t, plan.CurrentAttentionEnd)
}

func TestAgentHooksRuntimeOpenCodeOutputBound(t *testing.T) {
	home := isolateAgentHookHomes(t)
	installOpenCodeVersionFixture(t, "printf '%5000s' x")
	_, err := runHookSetup(t, "install", "opencode", "--executable", os.Args[0])
	require.ErrorContains(t, err, "4096 bytes")
	require.NoDirExists(t, filepath.Join(home, ".config", "opencode"))
}

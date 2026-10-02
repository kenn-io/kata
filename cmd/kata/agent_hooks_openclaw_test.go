package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func openClawTestOptions(t *testing.T) nativeAgentHookOptions {
	t.Helper()
	root := t.TempDir()
	return nativeAgentHookOptions{Agent: "openclaw", Scope: "user", Home: root, Dir: filepath.Join(root, "workspace"), Executable: "/usr/bin/kata", Contract: true, Attention: true}
}
func TestOpenClawObjectRejectsNilMap(t *testing.T) {
	parent := map[string]any{"hooks": map[string]any(nil)}
	value, err := openClawObject(parent, "hooks", true)
	require.Error(t, err)
	require.Nil(t, value)
}

func TestOpenClawObjectRejectsAbsentParentOnCreate(t *testing.T) {
	require.NotPanics(t, func() {
		value, err := openClawObject(nil, "hooks", true)
		require.Error(t, err)
		require.Nil(t, value)
	})
	value, err := openClawObject(nil, "hooks", false)
	require.NoError(t, err)
	require.Nil(t, value)
}

func TestOpenClawUninstallAbsentBundle(t *testing.T) {
	opts := openClawTestOptions(t)
	plan, err := planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.False(t, changed)
	for _, change := range plan.Changes {
		_, err := os.Stat(change.Path)
		require.True(t, os.IsNotExist(err))
	}
}

func TestOpenClawPlanBundleLifecycle(t *testing.T) {
	opts := openClawTestOptions(t)
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, p.Contract)
	require.True(t, p.AttentionStart)
	require.True(t, p.AttentionEnd)
	require.Len(t, p.Changes, 4)
	for _, c := range p.Changes {
		_, err = os.Stat(c.Path)
		require.True(t, os.IsNotExist(err), "planner wrote %s", c.Path)
	}
	changed, err := publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	require.True(t, changed)
	p, err = planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	changed, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	require.False(t, changed)
	opts.Attention = false
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, p.Contract)
	require.True(t, p.AttentionStart)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	p, err = planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, p.AttentionStart)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	opts.Attention = true
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, p.Contract)
	require.False(t, p.AttentionStart)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
}
func TestOpenClawPreservesPolicyAndRejectsUnsupportedConfig(t *testing.T) {
	for _, config := range []string{`{"$include":"other.json"}`, `{// comment\n}`, `{"plugins":{"entries":{"kata-hooks-user":{"enabled":false,"hooks":{"allowConversationAccess":false}}}},"foreign":7}`} {
		t.Run(config, func(t *testing.T) {
			opts := openClawTestOptions(t)
			opts.ConfigPath = filepath.Join(opts.Home, "config.json")
			require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(config), 0600))
			p, err := planOpenClawAgentHooks(opts, false)
			if config[2] == '$' || config[1] == '/' {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.False(t, p.Contract)
			require.False(t, p.AttentionStart)
			require.NotEmpty(t, p.Warnings)
			_, err = publishNativeAgentHookPlan(p)
			require.NoError(t, err)
			raw, err := os.ReadFile(opts.ConfigPath)
			require.NoError(t, err)
			var v map[string]any
			require.NoError(t, json.Unmarshal(raw, &v))
			require.Equal(t, float64(7), v["foreign"])
		})
	}
}
func TestOpenClawAuthoredEditsAreNeverOverwritten(t *testing.T) {
	opts := openClawTestOptions(t)
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	for _, c := range p.Changes {
		if filepath.Base(c.Path) == "index.js" {
			raw, err := os.ReadFile(c.Path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(c.Path, append(raw, []byte("\n// authored\n")...), 0600)) //nolint:gosec // G703: mutation of a planned artifact inside the isolated test home.
		}
	}
	_, err = planOpenClawAgentHooks(opts, false)
	require.Error(t, err)
	_, err = planOpenClawAgentHooks(opts, true)
	require.Error(t, err)
}
func TestOpenClawGeneratedNativeBehavior(t *testing.T) {
	for _, platform := range []string{"native", "windows-mode"} {
		t.Run(platform, func(t *testing.T) { openClawGeneratedNativeBehavior(t, platform) })
	}
}

func openClawGeneratedNativeBehavior(t *testing.T, platform string) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.Executable = filepath.Join(opts.Home, "record-kata")
	opts.Source = "prompt with spaces;literal.txt"
	opts.SourceSet = true
	opts.Dir = filepath.Join(opts.Home, "workspace")
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	opts.Executable = openClawInstallRecorder(t, opts.Executable)
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	plugin := ""
	for _, c := range p.Changes {
		if filepath.Base(c.Path) == "index.js" {
			plugin = c.Path
		}
	}
	cmd := exec.Command(node, "testdata/openclaw/behavior.mjs", plugin, opts.Dir) //nolint:gosec // G204: published Node executable runs fixed native fixture with isolated paths.
	cmd.Env = append(os.Environ(), "KATA_REF=abcd", "KATA_INBOX_USER=actor/worker", "OPENCLAW_TEST_ROOT="+opts.Home, "OPENCLAW_STATE_DIR="+filepath.Join(opts.Home, "state"), "OPENCLAW_TEST_PLATFORM="+platform, "TMPDIR="+opts.Home)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestOpenClawProjectPrecedenceAndCapturedLaunch(t *testing.T) {
	for _, attentionOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(attentionOnly), func(t *testing.T) { openClawProjectPrecedence(t, attentionOnly) })
	}
}
func openClawProjectPrecedence(t *testing.T, attentionOnly bool) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.Executable = filepath.Join(opts.Home, "record-kata")
	opts.Executable = openClawInstallRecorder(t, opts.Executable)
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	plugins := []string{}
	for _, scope := range []string{"user", "project"} {
		opts.Scope = scope
		p, err := planOpenClawAgentHooks(opts, false)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(p)
		require.NoError(t, err)
		if scope == "project" && attentionOnly {
			opts.Attention = false
			p, err = planOpenClawAgentHooks(opts, true)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(p)
			require.NoError(t, err)
			opts.Attention = true
		}
		for _, c := range p.Changes {
			if filepath.Base(c.Path) == "index.js" {
				plugins = append(plugins, c.Path)
			}
		}
	}
	cmd := exec.Command(node, append([]string{"testdata/openclaw/precedence.mjs"}, append(plugins, opts.Dir)...)...) //nolint:gosec // G204: published Node executable runs fixed native fixture with isolated paths.
	cmd.Env = append(os.Environ(), "KATA_REF=launch-ref", "KATA_INBOX_USER=actor/worker", "OPENCLAW_TEST_ROOT="+opts.Home, "OPENCLAW_TEST_ATTENTION_ONLY="+fmt.Sprint(attentionOnly), "TMPDIR="+opts.Home)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// Opt-in integration against the published, isolated OpenClaw installation.
func TestOpenClawPublishedNativeLoader(t *testing.T) {
	runtime := os.Getenv("KATA_OPENCLAW_TEST_RUNTIME")
	if runtime == "" {
		t.Skip("set KATA_OPENCLAW_TEST_RUNTIME to isolated published OpenClaw package")
	}
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	opts := openClawTestOptions(t)
	opts.Executable = filepath.Join(opts.Home, "record-kata")
	opts.Executable = openClawInstallRecorder(t, opts.Executable)
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	for name, text := range map[string]string{"contract": "native-contract", "inbox": "native-request"} {
		require.NoError(t, os.WriteFile(filepath.Join(opts.Home, name), []byte(text), 0600))
	}
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	cmd := exec.Command(node, "testdata/openclaw/published.mjs", runtime, p.Path, opts.Dir) //nolint:gosec // G204: published Node executable runs fixed native fixture with isolated paths.
	// Deliberately allow only OS/toolchain environment, then isolated runtime
	// targets and noncredential test attribution. No host daemon/provider env.
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "TEMP=" + opts.Home, "TMP=" + opts.Home, "USERPROFILE=" + opts.Home, "PATH=" + os.Getenv("PATH"), "HOME=" + opts.Home, "TMPDIR=" + opts.Home, "OPENCLAW_STATE_DIR=" + filepath.Join(opts.Home, ".openclaw"), "OPENCLAW_CONFIG_PATH=" + p.Path, "OPENCLAW_TEST_ROOT=" + opts.Home, "KATA_REF=abcd", "KATA_INBOX_USER=actor/worker"}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Log(string(out))
	if destination := os.Getenv("KATA_OPENCLAW_TEST_SAVE"); destination != "" {
		require.NoError(t, os.MkdirAll(destination, 0700)) //nolint:gosec // G703: native SDK fixture copy stays under the isolated test directory.
		for _, c := range p.Changes {
			relative, err := filepath.Rel(opts.Home, c.Path)
			require.NoError(t, err)
			target := filepath.Join(destination, relative)
			require.NoError(t, os.MkdirAll(filepath.Dir(target), 0700)) //nolint:gosec // G703: native SDK fixture copy stays under the isolated test directory.
			content := strings.ReplaceAll(string(c.Content), opts.Home, destination)
			require.NoError(t, os.WriteFile(target, []byte(content), 0600)) //nolint:gosec // G703: native fixture writes stay inside the isolated SDK and workspace paths.
		}
		openClawCopyRecorder(t, opts.Executable, filepath.Join(destination, filepath.Base(opts.Executable)))
		for name, text := range map[string]string{"contract": "native-contract", "inbox": "native-request"} {
			require.NoError(t, os.WriteFile(filepath.Join(destination, name), []byte(text), 0600)) //nolint:gosec // G703: native fixture writes stay inside the isolated SDK and workspace paths.
		}
	}
}

func TestOpenClawConfigRetainsAuthoredNumbersAndAttentionDenial(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.ConfigPath = filepath.Join(opts.Home, "config.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"foreign":{"large":9007199254740993},"plugins":{"allow":["foreign-plugin"],"entries":{"foreign-plugin":{"enabled":true}}}}`), 0600))
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	var config struct {
		Foreign struct{ Large json.RawMessage }
	}
	require.NoError(t, json.Unmarshal(data, &config))
	require.Equal(t, "9007199254740993", string(config.Foreign.Large))
	opts.Attention = false
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, p.Contract)
	require.True(t, p.AttentionStart)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	data, err = os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	var all map[string]any
	require.NoError(t, json.Unmarshal(data, &all))
	plugins := all["plugins"].(map[string]any)
	entries := plugins["entries"].(map[string]any)
	entry := entries["kata-hooks-user"].(map[string]any)
	entry["hooks"].(map[string]any)["allowPromptInjection"] = false
	data, err = json.Marshal(all)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(opts.ConfigPath, data, 0600))
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, p.CurrentAttentionStart)
	require.False(t, p.AttentionStart)
	require.False(t, p.AttentionEnd)
	require.NotEmpty(t, p.Warnings)
}

func TestOpenClawNativeRootOverrides(t *testing.T) {
	for _, mode := range []string{"state", "config", "home", "profile", "readonly"} {
		t.Run(mode, func(t *testing.T) {
			opts := openClawTestOptions(t)
			for _, key := range []string{"OPENCLAW_STATE_DIR", "OPENCLAW_CONFIG_PATH", "OPENCLAW_HOME", "OPENCLAW_PROFILE", "OPENCLAW_NIX_MODE", "OPENCLAW_CONFIG_READONLY"} {
				t.Setenv(key, "")
			}
			target := filepath.Join(opts.Home, "selected")
			want := target
			switch mode {
			case "state":
				t.Setenv("OPENCLAW_STATE_DIR", target)
			case "config":
				t.Setenv("OPENCLAW_CONFIG_PATH", filepath.Join(target, "custom.json"))
			case "home":
				t.Setenv("OPENCLAW_HOME", target)
				want = filepath.Join(target, ".openclaw")
			case "profile":
				t.Setenv("OPENCLAW_PROFILE", "example")
				want = filepath.Join(opts.Home, ".openclaw-example")
			case "readonly":
				t.Setenv("OPENCLAW_CONFIG_READONLY", "1")
			}
			p, err := planOpenClawAgentHooks(opts, false)
			if mode == "readonly" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.Join(want, "extensions", "kata-hooks-user", "index.js"), p.Changes[0].Path)
		})
	}
}

func TestOpenClawReadOnlyReplanKeepsPinnedExecutableAndSource(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.SourceSet = true
	opts.Source = "custom prompt.txt"
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	opts.Executable = ""
	opts.Source = ""
	opts.SourceSet = false
	opts.Attention = false
	p, err = planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, p.CurrentContract)
	require.True(t, p.CurrentAttentionStart)
	changed, err := publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestOpenClawStatusDoesNotProposeMutations(t *testing.T) {
	opts := openClawTestOptions(t)
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	opts.Contract = false
	opts.Attention = false
	opts.Executable = ""
	t.Setenv("OPENCLAW_CONFIG_READONLY", "1")
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.True(t, p.CurrentContract)
	require.True(t, p.CurrentAttentionStart)
	for _, c := range p.Changes {
		require.False(t, c.Remove)
		require.Equal(t, c.Original, c.Content)
	}
	t.Setenv("OPENCLAW_CONFIG_READONLY", "")
	opts.Home = t.TempDir()
	p, err = planOpenClawAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, p.CurrentContract)
	changed, err := publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestOpenClawSessionEndBoundsQueuedCleanup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.Contract = false
	opts.Executable = filepath.Join(opts.Home, "record-kata")
	opts.Executable = openClawInstallRecorder(t, opts.Executable)
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	p, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(p)
	require.NoError(t, err)
	cmd := exec.Command(node, "testdata/openclaw/timeout.mjs", p.Changes[0].Path, opts.Dir) //nolint:gosec // G204: published Node executable runs fixed native fixture with isolated paths.
	cmd.Env = append(os.Environ(), "KATA_REF=abcd", "OPENCLAW_TEST_ROOT="+opts.Home, "TMPDIR="+opts.Home)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestOpenClawTerminalFallbackWithoutAnotherPrompt(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, transition := range []string{"dispose", "deny"} {
		for _, reason := range []string{"reset", "new"} {
			t.Run(transition+"/"+reason, func(t *testing.T) {
				opts := openClawTestOptions(t)
				opts.Contract = false
				require.NoError(t, os.MkdirAll(opts.Dir, 0700))
				plugins := []string{}
				executables := []string{}
				for _, scope := range []string{"user", "project"} {
					opts.Scope = scope
					opts.Executable = filepath.Join(opts.Home, scope+"-record-kata")
					opts.Executable = openClawInstallRecorder(t, opts.Executable)
					executables = append(executables, opts.Executable)
					p, err := planOpenClawAgentHooks(opts, false)
					require.NoError(t, err)
					_, err = publishNativeAgentHookPlan(p)
					require.NoError(t, err)
					plugins = append(plugins, p.Changes[0].Path)
				}
				cmd := exec.Command(node, "testdata/openclaw/terminal-fallback.mjs", plugins[0], plugins[1], opts.Dir, executables[1], transition, reason) //nolint:gosec // G204: published Node executable runs fixed native fixture with isolated paths.
				cmd.Env = append(os.Environ(), "KATA_REF=captured-ref", "OPENCLAW_TEST_ROOT="+opts.Home, "TMPDIR="+opts.Home)
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, string(out))
			})
		}
	}
}

func TestOpenClawRecorderDoesNotRequireChildNodePATH(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.Executable = filepath.Join(opts.Home, "record-kata")
	opts.Executable = openClawInstallRecorder(t, opts.Executable)
	require.NoError(t, os.WriteFile(filepath.Join(opts.Home, "contract"), []byte("recorded"), 0600))
	cmd := exec.Command(opts.Executable, "agent-contract-hook") //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
	cmd.Dir = opts.Home
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "OPENCLAW_TEST_ROOT=" + opts.Home, "PATH=" + filepath.Join(opts.Home, "empty")}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	var response struct {
		HookSpecificOutput struct{ AdditionalContext string }
	}
	require.NoError(t, json.Unmarshal(out, &response))
	require.Equal(t, "contract:recorded", response.HookSpecificOutput.AdditionalContext)
}

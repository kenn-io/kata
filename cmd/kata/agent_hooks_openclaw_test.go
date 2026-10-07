package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func openClawTestOptions(t *testing.T) nativeAgentHookOptions {
	t.Helper()
	root := t.TempDir()
	return nativeAgentHookOptions{Agent: "openclaw", Scope: "user", Home: root, Dir: filepath.Join(root, "workspace"), Executable: "/usr/bin/kata", Contract: true, Attention: true}
}

func TestOpenClawContractSurvivesSlowAgentTelemetry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	env, workspace, _ := setupCLIWorkspace(t)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/ui/telemetry" {
			var body struct{ Event string }
			if json.NewDecoder(r.Body).Decode(&body) == nil && body.Event == "agent_active" {
				posts.Add(1)
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer close(release)
	opts := openClawTestOptions(t)
	opts.Attention = false
	opts.Dir = workspace
	opts.Executable = filepath.Join(opts.Home, "kata")
	if runtime.GOOS == "windows" {
		opts.Executable += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "kit_posthog_disabled", "-buildvcs=false", "-o", opts.Executable, "go.kenn.io/kata/cmd/kata") //nolint:gosec // G204: fixed build arguments produce an isolated test executable.
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	plan, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	contract := filepath.Join(opts.Home, "contract")
	require.NoError(t, os.WriteFile(contract, []byte(agentContractText), 0600))
	cmd := exec.CommandContext(t.Context(), node, "testdata/openclaw/slow-telemetry.mjs", plan.Changes[0].Path, workspace, contract) //nolint:gosec // G204: Node runs a fixed fixture with the generated plugin and test-owned executable.
	cmd.Env = []string{"SystemRoot=" + os.Getenv("SystemRoot"), "PATH=" + os.Getenv("PATH"), "HOME=" + opts.Home, "USERPROFILE=" + opts.Home, "TEMP=" + opts.Home, "TMP=" + opts.Home, "TMPDIR=" + opts.Home, "KATA_HOME=" + opts.Home, "KATA_SERVER=" + server.URL, "KATA_AUTHOR=user-a"}
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Equal(t, int32(1), posts.Load(), "contract sends agent activity")
}

func TestOpenClawGeneratedSourceRemainsData(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, separator := range []string{"\u2028", "\u2029"} {
		opts := openClawTestOptions(t)
		opts.SourceSet = true
		opts.Source = opts.Home + string(os.PathSeparator) + separator + "globalThis.KATA_REVIEW_MARKER=1;//"
		plan, err := planOpenClawAgentHooks(opts, false)
		require.NoError(t, err)
		extension := filepath.Join(t.TempDir(), "extension.mjs")
		require.NoError(t, os.WriteFile(extension, plan.Changes[0].Content, 0600))
		//nolint:gosec // G204: Node imports only the generated fixture in the temporary directory.
		command := exec.Command(node, "--input-type=module", "-e", `
import assert from 'node:assert/strict';
import {pathToFileURL} from 'node:url';
const plugin = await import(pathToFileURL(process.argv[1]));
assert.equal(typeof plugin.default.register, 'function');
assert.equal(globalThis.KATA_REVIEW_MARKER, undefined, 'source escaped its metadata comment');
`, extension)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
}

func TestOpenClawInboxIsOffByDefault(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.Executable = openClawInstallRecorder(t, filepath.Join(opts.Home, "record-kata"))
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	plan, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(opts.Home, "contract"), []byte("default-contract"), 0600))

	var plugin string
	for _, change := range plan.Changes {
		if filepath.Base(change.Path) == "index.js" {
			plugin = change.Path
		}
	}
	cmd := exec.Command(node, "testdata/openclaw/inbox-opt-in.mjs", plugin, opts.Dir) //nolint:gosec // G204: Node runs a fixed fixture against isolated paths.
	cmd.Env = append(os.Environ(), "KATA_REF=abcd", "KATA_INBOX_USER=actor/worker", "OPENCLAW_TEST_ROOT="+opts.Home, "TMPDIR="+opts.Home)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestOpenClawShareInboxCLIIsExplicitAndSingleTarget(t *testing.T) {
	home := isolateAgentHookHomes(t)
	t.Setenv("KATA_INBOX_USER", "actor/worker")
	resetFlags(t)
	out, diagnostic, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "OpenClaw", "--share-inbox", "--executable", os.Args[0], "--json")
	require.NoError(t, err, diagnostic)
	require.Contains(t, out, "every prompt handled by the OpenClaw plugin")

	plugin := filepath.Join(home, ".openclaw", "extensions", "kata-hooks-user", "index.js")
	data, err := os.ReadFile(plugin) //nolint:gosec // G304: generated artifact is inside the isolated test home.
	require.NoError(t, err)
	line, _, ok := strings.Cut(string(data), "\n")
	require.True(t, ok)
	var managed struct {
		ShareInbox bool `json:"shareInbox"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &managed))
	require.True(t, managed.ShareInbox)
	require.NotContains(t, string(data), "actor/worker")

	resetFlags(t)
	_, diagnostic, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "openclaw", "--executable", os.Args[0])
	require.NoError(t, err, diagnostic)
	data, err = os.ReadFile(plugin) //nolint:gosec // G304: generated artifact is inside the isolated test home.
	require.NoError(t, err)
	line, _, ok = strings.Cut(string(data), "\n")
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &managed))
	require.True(t, managed.ShareInbox, "an install without the option preserves the existing choice")

	resetFlags(t)
	_, diagnostic, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "openclaw", "--share-inbox=false", "--executable", os.Args[0])
	require.NoError(t, err, diagnostic)
	data, err = os.ReadFile(plugin) //nolint:gosec // G304: generated artifact is inside the isolated test home.
	require.NoError(t, err)
	line, _, ok = strings.Cut(string(data), "\n")
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &managed))
	require.False(t, managed.ShareInbox)

	for name, args := range map[string][]string{
		"implicit discovery": {"agent-hook", "install", "--share-inbox", "--executable", os.Args[0]},
		"all targets":        {"agent-hook", "install", "--all", "--share-inbox", "--executable", os.Args[0]},
		"other target":       {"agent-hook", "install", "pi", "--share-inbox", "--executable", os.Args[0]},
		"multiple targets":   {"agent-hook", "install", "openclaw", "pi", "--share-inbox", "--executable", os.Args[0]},
	} {
		t.Run(name, func(t *testing.T) {
			resetFlags(t)
			_, diagnostic, err := executeAgentHook(t, strings.NewReader(""), args...)
			require.ErrorContains(t, err, "--share-inbox requires exactly one explicit OpenClaw target")
			require.NotContains(t, diagnostic, "publish hook bundle")
		})
	}

	resetFlags(t)
	_, diagnostic, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "nosuchtool", "--share-inbox", "--executable", os.Args[0])
	require.ErrorContains(t, err, `unknown harness "nosuchtool"`)
	require.NotContains(t, diagnostic, "--share-inbox requires exactly one explicit OpenClaw target")
}

func TestOpenClawLegacyManagedMetadataUpgradesWithSharingOff(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.ShareInbox = true
	opts.ShareInboxSet = true
	initial, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(initial)
	require.NoError(t, err)

	// Pre-option bundles lack a recorded choice even though their runtime read
	// KATA_INBOX_USER whenever it was set. Recreate the older marker while
	// retaining the valid ownership digest so the upgrade path is exercised.
	code, err := os.ReadFile(initial.Changes[0].Path)
	require.NoError(t, err)
	line, remainder, ok := strings.Cut(string(code), "\n")
	require.True(t, ok)
	var metadata map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &metadata))
	delete(metadata, "shareInbox")
	legacyLine, err := json.Marshal(metadata)
	require.NoError(t, err)
	digestIndex := strings.LastIndex(remainder, "\n"+nativeAgentHookDigestPrefix)
	require.NotEqual(t, -1, digestIndex)
	legacyBody := []byte(openClawManagedPrefix + string(legacyLine) + "\n" + remainder[:digestIndex+1])
	legacyCode := sealNativeAgentHookCode(legacyBody, initial.Changes[1].Content, initial.Changes[2].Content)
	//nolint:gosec // G703: the planner path is inside this test's isolated OpenClaw home.
	require.NoError(t, os.WriteFile(initial.Changes[0].Path, legacyCode, 0600))

	statusOpts := opts
	statusOpts.Contract = false
	statusOpts.Attention = false
	status, err := planOpenClawAgentHooks(statusOpts, true)
	require.NoError(t, err)
	statusWarnings := strings.Join(status.Warnings, "\n")
	require.Contains(t, statusWarnings, "legacy OpenClaw plugin")
	require.Contains(t, statusWarnings, "until it is reinstalled")

	opts.ShareInbox = false
	opts.ShareInboxSet = false
	upgraded, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	upgradeWarnings := strings.Join(upgraded.Warnings, "\n")
	require.Contains(t, upgradeWarnings, "turning off the legacy inbox sharing behavior")
	line, _, ok = strings.Cut(string(upgraded.Changes[0].Content), "\n")
	require.True(t, ok)
	var managed openClawManaged
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &managed))
	require.False(t, managed.ShareInbox)
}

func TestOpenClawStatusWarnsWhenSharedInboxIsEnabled(t *testing.T) {
	installOpts := openClawTestOptions(t)
	installOpts.ShareInbox = true
	installOpts.ShareInboxSet = true
	installed, err := planOpenClawAgentHooks(installOpts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(installed)
	require.NoError(t, err)

	statusOpts := installOpts
	statusOpts.Contract = false
	statusOpts.Attention = false
	statusOpts.ShareInbox = false
	statusOpts.ShareInboxSet = false
	status, err := planOpenClawAgentHooks(statusOpts, true)
	require.NoError(t, err)
	require.Contains(t, strings.Join(status.Warnings, "\n"), "OpenClaw shared inbox is enabled")
	require.Contains(t, strings.Join(status.Warnings, "\n"), "--share-inbox=false")
}

func TestOpenClawContractRemovalClearsSharedInboxChoice(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.ShareInbox = true
	opts.ShareInboxSet = true
	installed, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(installed)
	require.NoError(t, err)

	removeOpts := opts
	// Removing the contract leaves the independent attention registration intact.
	removeOpts.Attention = false
	removeOpts.ShareInboxSet = false
	removed, err := planOpenClawAgentHooks(removeOpts, true)
	require.NoError(t, err)
	line, _, ok := strings.Cut(string(removed.Changes[0].Content), "\n")
	require.True(t, ok)
	var removedMetadata openClawManaged
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &removedMetadata))
	require.False(t, removedMetadata.Contract)
	require.True(t, removedMetadata.Attention)
	require.False(t, removedMetadata.ShareInbox)
	_, err = publishNativeAgentHookPlan(removed)
	require.NoError(t, err)

	reinstallOpts := opts
	reinstallOpts.ShareInbox = false
	reinstallOpts.ShareInboxSet = false
	reinstalled, err := planOpenClawAgentHooks(reinstallOpts, false)
	require.NoError(t, err)
	line, _, ok = strings.Cut(string(reinstalled.Changes[0].Content), "\n")
	require.True(t, ok)
	var managed openClawManaged
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, openClawManagedPrefix)), &managed))
	require.False(t, managed.ShareInbox)
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

func TestOpenClawPreservesEmptyPluginAllowList(t *testing.T) {
	opts := openClawTestOptions(t)
	opts.ConfigPath = filepath.Join(opts.Home, "config.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"plugins":{"allow":[]}}`), 0600))

	plan, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, plan.Contract)
	require.True(t, plan.AttentionStart)
	require.True(t, plan.AttentionEnd)

	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.True(t, changed)

	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(data, &config))
	plugins, ok := config["plugins"].(map[string]any)
	require.True(t, ok)
	require.Empty(t, plugins["allow"])
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
	t.Run("large contract", func(t *testing.T) {
		openClawGeneratedNativeBehaviorWithContract(t, "native", strings.Repeat("x", 96*1024))
	})
}

func TestOpenClawSubagentSessionsDoNotOwnAttention(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.Executable = openClawInstallRecorder(t, filepath.Join(opts.Home, "record-kata"))
	require.NoError(t, os.MkdirAll(opts.Dir, 0700))
	plan, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	var plugin string
	for _, change := range plan.Changes {
		if filepath.Base(change.Path) == "index.js" {
			plugin = change.Path
		}
	}

	cmd := exec.Command(node, "testdata/openclaw/subagent.mjs", plugin, opts.Dir) //nolint:gosec // G204: published Node executable runs a fixed lifecycle fixture against isolated paths.
	cmd.Env = append(os.Environ(), "KATA_REF=launch-ref", "OPENCLAW_TEST_ROOT="+opts.Home, "TMPDIR="+opts.Home)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func openClawGeneratedNativeBehavior(t *testing.T, platform string) {
	openClawGeneratedNativeBehaviorWithContract(t, platform, "first")
}

func openClawGeneratedNativeBehaviorWithContract(t *testing.T, platform, contract string) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	opts := openClawTestOptions(t)
	opts.ShareInbox, opts.ShareInboxSet = true, true
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
	require.NoError(t, os.WriteFile(filepath.Join(opts.Home, "contract"), []byte(contract), 0600))
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
	opts.ShareInbox, opts.ShareInboxSet = true, true
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

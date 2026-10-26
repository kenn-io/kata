package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// These tests catch writing outside the selected native root, deleting an
// attention-only adapter, and adopting authored or edited extension code.
func ompTestOptions(t *testing.T) nativeAgentHookOptions {
	t.Helper()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	return nativeAgentHookOptions{Agent: "omp", Scope: "project", Home: t.TempDir(), Dir: t.TempDir(), Executable: "kata", Contract: true}
}
func TestOmpNativePlanningAndOwnership(t *testing.T) {
	opts := ompTestOptions(t)
	opts.Attention = true
	plan, err := planOmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(opts.Dir, ".omp", "extensions", "kata.js")
	if plan.Path != want || !plan.Contract || !plan.AttentionStart || !plan.AttentionEnd || plan.CurrentContract {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if changed, err := publishNativeAgentHookPlan(plan); err != nil || !changed {
		t.Fatalf("publish: %v %v", changed, err)
	}
	original, err := os.ReadFile(want) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if err != nil {
		t.Fatal(err)
	}
	opts.Attention = false
	plan, err = planOmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CurrentContract || !plan.CurrentAttentionStart || !plan.CurrentAttentionEnd {
		t.Fatalf("missing observed state: %+v", plan)
	}
	if changed, err := publishNativeAgentHookPlan(plan); err != nil || changed {
		t.Fatalf("reinstall: %v %v", changed, err)
	}
	plan, err = planOmpAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Contract || !plan.AttentionStart || !plan.AttentionEnd {
		t.Fatalf("default removal loses attention: %+v", plan)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(want) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if bytes.Equal(original, after) {
		t.Fatal("contract was not removed")
	}
	opts.Attention = true
	plan, err = planOmpAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("artifact not removed: %v", err)
	}
}
func TestOmpNativeAuthoredAndEditedFilesPreserved(t *testing.T) {
	for _, body := range []string{"export default function authored(pi) {}\n", "// kata-native-omp {broken}\n"} {
		t.Run(body, func(t *testing.T) {
			opts := ompTestOptions(t)
			path := filepath.Join(opts.Dir, ".omp", "extensions", "kata.js")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, remove := range []bool{false, true} {
				if _, err := planOmpAgentHooks(opts, remove); err == nil {
					t.Fatal("authored collision accepted")
				}
			}
			got, _ := os.ReadFile(path) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if string(got) != body {
				t.Fatal("authored file modified")
			}
		})
	}
	opts := ompTestOptions(t)
	plan, err := planOmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(plan.Path)
	data = append(data, []byte("// local edit\n")...)
	if err = os.WriteFile(plan.Path, data, 0600); err != nil { //nolint:gosec // G703: native fixture writes stay inside the isolated SDK and workspace paths.
		t.Fatal(err)
	}
	if _, err = planOmpAgentHooks(opts, true); err == nil {
		t.Fatal("edited generated file accepted")
	}
	after, err := os.ReadFile(plan.Path)
	require.NoError(t, err)
	require.Equal(t, data, after)
}

func TestOmpPiSharedDirectoryCollision(t *testing.T) {
	for _, first := range []string{"pi", "omp"} {
		t.Run(first, func(t *testing.T) {
			opts := ompTestOptions(t)
			opts.Scope = "user"
			root := t.TempDir()
			t.Setenv("PI_CODING_AGENT_DIR", root)
			fakeOmp(t, root+"\n", nil)
			opts.Agent = first
			plan, err := planNativeAgentHooks(opts, false)
			require.NoError(t, err)
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			before, err := os.ReadFile(plan.Path)
			require.NoError(t, err)
			if first == "pi" {
				opts.Agent = "omp"
			} else {
				opts.Agent = "pi"
			}
			_, err = planNativeAgentHooks(opts, false)
			require.ErrorContains(t, err, "PI_CODING_AGENT_DIR")
			after, err := os.ReadFile(plan.Path)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
func drawOmpMetadata(ht *hegel.T) ompAgentHookMetadata {
	return ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: hegel.Draw(ht, hegel.SampledFrom([]string{"user", "project"})), Workspace: hegel.Draw(ht, hegel.Text()), Executable: hegel.Draw(ht, hegel.Text().MinSize(1)), Source: hegel.Draw(ht, hegel.Text()), SourceSet: hegel.Draw(ht, hegel.Booleans()), Contract: hegel.Draw(ht, hegel.Booleans()), Attention: hegel.Draw(ht, hegel.Booleans()), Plan: hegel.Draw(ht, hegel.Booleans())}
}
func TestOmpOwnershipRejectsBodyEditProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		data, err := generateOmpAgentHooks(drawOmpMetadata(ht))
		if err != nil {
			ht.Fatal(err)
		}
		start := bytes.IndexByte(data, '\n') + 1
		index := hegel.Draw(ht, hegel.Integers(start, len(data)-1))
		data[index] ^= byte(hegel.Draw(ht, hegel.Integers(1, 255))) //nolint:gosec // G115: generated byte values are explicitly limited to [1, 255].
		_, err = parseOmpAgentHookMetadata(data)
		if err == nil || !strings.Contains(err.Error(), "edited") {
			ht.Fatalf("edit at %d accepted: %v", index, err)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
func TestOmpNativeGeneratedExecution(t *testing.T) {
	opts := ompTestOptions(t)
	opts.Attention = true
	opts.Source = "contract '$;`.txt"
	opts.SourceSet = true
	runOmpFixture(t, opts, "omp.mjs")
}
func runOmpFixture(t *testing.T, opts nativeAgentHookOptions, fixture string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	plan, err := planOmpAgentHooks(opts, false)
	require.NoError(t, err)
	extension := filepath.Join(t.TempDir(), "kata.mjs")
	require.NoError(t, os.WriteFile(extension, plan.Changes[0].Content, 0600))
	pi, err := generatePiAgentHooks(piAgentHookMetadata{Format: "kata-pi", Version: 1, Scope: "user", Executable: "kata", Attention: true, Contract: true})
	require.NoError(t, err)
	piPath := filepath.Join(t.TempDir(), "pi.mjs")
	require.NoError(t, os.WriteFile(piPath, pi, 0600))
	user, err := generateOmpAgentHooks(ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: "user", Executable: "user-kata", Source: "user.txt", SourceSet: true, Contract: true, Attention: true})
	require.NoError(t, err)
	userPath := filepath.Join(t.TempDir(), "user.mjs")
	require.NoError(t, os.WriteFile(userPath, user, 0600))
	nested, err := generateOmpAgentHooks(ompAgentHookMetadata{Format: "kata-omp", Version: 1, Scope: "project", Workspace: filepath.Join(opts.Dir, "nested"), Executable: "nested-kata", Contract: true})
	require.NoError(t, err)
	nestedPath := filepath.Join(t.TempDir(), "nested.mjs")
	require.NoError(t, os.WriteFile(nestedPath, nested, 0600))
	cmd := exec.Command(node, filepath.Join("testdata", "agent-hooks", fixture), extension, opts.Dir, piPath, userPath, nestedPath) //nolint:gosec // G204: fixed local Node fixture and isolated temporary paths.
	cmd.Env = append(os.Environ(), "KATA_REF=issue-ref", "KATA_INBOX_USER=actor/teammate", "KATA_ATTENTION_HOST=")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
func TestOmpAdapterIgnoresPiHost(t *testing.T) {
	opts := ompTestOptions(t)
	runOmpFixture(t, opts, "omp-host-guard.mjs")
}
func TestOmpNativeAuthoredSiblingPreserved(t *testing.T) {
	opts := ompTestOptions(t)
	sibling := filepath.Join(opts.Dir, ".omp", "extensions", "kata.ts")
	require.NoError(t, os.MkdirAll(filepath.Dir(sibling), 0700))
	require.NoError(t, os.WriteFile(sibling, []byte("authored"), 0600))
	_, err := planOmpAgentHooks(opts, false)
	require.ErrorContains(t, err, "kata.ts")
	got, err := os.ReadFile(sibling) //nolint:gosec // G304: authored sibling is created under the isolated temporary extension root.
	require.NoError(t, err)
	require.Equal(t, "authored", string(got))
}
func TestOmpStatusAndUninstall(t *testing.T) {
	home := isolateAgentHookHomes(t)
	root := filepath.Join(home, "omp-root")
	fakeOmp(t, root+"\n", nil)
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp", "--attention", "--source", "prompt.md", "--executable", os.Args[0])
	require.NoError(t, err)
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "omp", "--json")
	require.NoError(t, err)
	var report agentHookStatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.Len(t, report.Harnesses, 1)
	status := report.Harnesses[0]
	require.Equal(t, "omp", status.Harness)
	require.Equal(t, "user", status.Scope)
	require.Equal(t, filepath.Join(root, "extensions", "kata.js"), status.User.ConfigPath)
	require.True(t, status.Configured.Contract)
	require.True(t, status.Configured.AttentionStart)
	require.True(t, status.Configured.AttentionEnd)
	resetFlags(t)
	_, _, err = executeAgentHook(t, strings.NewReader(""), "agent-hook", "uninstall", "omp", "--contract-only")
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(root, "extensions", "kata.js")) //nolint:gosec // G304: generated extension is under the test's temporary root.
	require.NoError(t, err)
	meta, err := parseOmpAgentHookMetadata(data)
	require.NoError(t, err)
	require.False(t, meta.Contract)
	require.True(t, meta.Attention)
}
func TestOmpNativeRealKataTransport(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	opts := ompTestOptions(t)
	opts.Attention = true
	opts.SourceSet = true
	opts.Source = "contract '$;`.txt"
	binary := filepath.Join(t.TempDir(), "kata")
	if runtime.GOOS == "windows" && filepath.Ext(binary) != ".exe" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, ".") //nolint:gosec // G204: fixed Go build target and isolated temporary output; inherits test limits.
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	opts.Executable = binary
	plan, err := planOmpAgentHooks(opts, false)
	require.NoError(t, err)
	extension := filepath.Join(t.TempDir(), "kata.mjs")
	require.NoError(t, os.WriteFile(extension, plan.Changes[0].Content, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(opts.Dir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"example-workspace\"\n"), 0600))
	command := exec.Command(node, "testdata/agent-hooks/omp-transport.mjs", extension, opts.Dir, filepath.Join(opts.Dir, opts.Source), binary) //nolint:gosec // G204: fixed local Node fixture and isolated temporary paths.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KATA_") && !strings.HasPrefix(entry, "CLAUDE_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "KATA_HOME="+t.TempDir(), "CLAUDE_PROJECT_DIR="+t.TempDir())
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestOmpAuthoredSiblingAppearingAfterPlanningPreserved(t *testing.T) {
	opts := ompTestOptions(t)
	plan, err := planOmpAgentHooks(opts, false)
	require.NoError(t, err)
	sibling := filepath.Join(filepath.Dir(plan.Path), "kata.ts")
	require.NoError(t, os.MkdirAll(filepath.Dir(sibling), 0700))
	require.NoError(t, os.WriteFile(sibling, []byte("authored"), 0600))
	changed, err := publishNativeAgentHookPlan(plan)
	require.ErrorContains(t, err, "changed after planning")
	require.False(t, changed)
	got, err := os.ReadFile(sibling) //nolint:gosec // G304: authored sibling is created under the isolated temporary extension root.
	require.NoError(t, err)
	require.Equal(t, "authored", string(got))
	_, err = os.Stat(plan.Path)
	require.True(t, os.IsNotExist(err))
}

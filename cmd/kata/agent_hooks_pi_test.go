package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests catch writing outside the selected native root, deleting an
// attention-only adapter, and adopting authored or edited extension code.
func piTestOptions(t *testing.T) nativeAgentHookOptions {
	t.Helper()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	return nativeAgentHookOptions{Agent: "pi", Scope: "project", Home: t.TempDir(), Dir: t.TempDir(), Executable: "kata", Contract: true}
}
func TestPiNativePlanningAndOwnership(t *testing.T) {
	opts := piTestOptions(t)
	opts.Attention = true
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(opts.Dir, ".pi", "extensions", "kata.js")
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
	plan, err = planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CurrentContract || !plan.CurrentAttentionStart || !plan.CurrentAttentionEnd {
		t.Fatalf("missing observed state: %+v", plan)
	}
	if changed, err := publishNativeAgentHookPlan(plan); err != nil || changed {
		t.Fatalf("reinstall: %v %v", changed, err)
	}
	plan, err = planPiAgentHooks(opts, true)
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
	plan, err = planPiAgentHooks(opts, true)
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
func TestPiNativeAuthoredAndEditedFilesPreserved(t *testing.T) {
	for _, body := range []string{"export default function authored(pi) {}\n", "// kata-native-pi {broken}\n"} {
		t.Run(body, func(t *testing.T) {
			opts := piTestOptions(t)
			path := filepath.Join(opts.Dir, ".pi", "extensions", "kata.js")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			for _, remove := range []bool{false, true} {
				if _, err := planPiAgentHooks(opts, remove); err == nil {
					t.Fatal("authored collision accepted")
				}
			}
			got, _ := os.ReadFile(path) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if string(got) != body {
				t.Fatal("authored file modified")
			}
		})
	}
	opts := piTestOptions(t)
	plan, err := planPiAgentHooks(opts, false)
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
	if _, err = planPiAgentHooks(opts, true); err == nil {
		t.Fatal("edited generated file accepted")
	}
}
func TestPiNativeUserRootAndUnsupportedOverrides(t *testing.T) {
	opts := piTestOptions(t)
	opts.Scope = "user"
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", root)
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != filepath.Join(root, "extensions", "kata.js") {
		t.Fatal(plan.Path)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "relative-root")
	if _, err = planPiAgentHooks(opts, false); err == nil {
		t.Fatal("relative agent root accepted")
	}
	t.Setenv("PI_CODING_AGENT_DIR", root)
	opts.ConfigPath = filepath.Join(root, "settings.json")
	if _, err = planPiAgentHooks(opts, false); err == nil {
		t.Fatal("config override accepted")
	}
}
func TestPiNativeGeneratedExecution(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for native Pi execution fixture")
	}
	opts := piTestOptions(t)
	opts.Attention = true
	opts.Executable = "/example path/kata '$;`"
	opts.Source = "contract '$;`.txt"
	opts.SourceSet = true
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "project.mjs")
	if err = os.WriteFile(path, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Source = "user.txt"
	user, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	userPath := filepath.Join(filepath.Dir(path), "user.mjs")
	if err = os.WriteFile(userPath, user.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/pi.mjs", path, userPath, opts.Dir) //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
	command.Env = append(os.Environ(), "KATA_REF=issue-ref", "KATA_INBOX_USER=actor/teammate", "KATA_SERVER=https://daemon.example", "KATA_AUTHOR=actor", "KATA_TEAMMATE=teammate")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native Pi execution: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Pi native fixture passed") {
		t.Fatalf("missing evidence: %s", output)
	}
}
func TestPiNativeProjectSourceUsesWorkspaceFromNestedCWD(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native executable fixture uses a Unix shebang")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for native Pi execution fixture")
	}

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	nested := filepath.Join(workspace, "nested")
	for _, dir := range []string{
		filepath.Join(workspace, "prompts"),
		filepath.Join(nested, "prompts"),
	} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(workspace, "prompts", "contract.md"), []byte("workspace contract"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(nested, "prompts", "contract.md"), []byte("nested decoy"), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "kata")
	reader, err := os.ReadFile("testdata/agent-hooks/native-source-reader.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(executable, reader, 0700); err != nil { //nolint:gosec // G306: the temporary fixture must be executable for the native process runner.
		t.Fatal(err)
	}
	opts := nativeAgentHookOptions{
		Agent: "pi", Scope: "project", Home: root, Dir: workspace,
		Executable: executable, Contract: true,
		Source: "prompts/contract.md", SourceSet: true,
	}
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(root, "kata.mjs")
	if err = os.WriteFile(extension, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/pi-project-source.mjs", extension, executable, workspace, nested) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
	command.Env = append(os.Environ(), "KATA_INBOX_USER=")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("nested Pi project source: %v\n%s", err, output)
	}
}

func TestPiNativeAttentionRetriesFailures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for native Pi execution fixture")
	}
	for _, mode := range []string{"start", "end"} {
		for _, failure := range []string{"nonzero", "killed", "reject"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				opts := piTestOptions(t)
				opts.Scope = "user"
				opts.Contract = false
				opts.Attention = true
				plan, err := planPiAgentHooks(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				extension := filepath.Join(t.TempDir(), "extension.mjs")
				if err = os.WriteFile(extension, plan.Changes[0].Content, 0600); err != nil {
					t.Fatal(err)
				}
				command := exec.Command(node, "testdata/agent-hooks/pi-attention-retry.mjs", extension, opts.Dir, mode, failure) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("Pi attention %s/%s: %v\n%s", mode, failure, err, output)
				}
			})
		}
	}
}

func FuzzPiNativeOwnershipRejectsEdits(f *testing.F) {
	f.Add("// changed\n")
	f.Add("\x00")
	f.Add("export default {}")
	f.Fuzz(func(t *testing.T, edit string) {
		if edit == "" {
			return
		}
		opts := piTestOptions(t)
		plan, err := planPiAgentHooks(opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Dir(plan.Path), 0700); err != nil {
			t.Fatal(err)
		}
		data := append(bytes.Clone(plan.Changes[0].Content), []byte(edit)...)
		if err = os.WriteFile(plan.Path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = planPiAgentHooks(opts, true); err == nil {
			t.Fatal("edited adapter was considered owned")
		}
	})
}

func TestPiNativeSiblingAndSymlinksPreserved(t *testing.T) {
	opts := piTestOptions(t)
	root := filepath.Join(opts.Dir, ".pi", "extensions")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "kata.ts")
	if err := os.WriteFile(sibling, []byte("export default function authored() {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := planPiAgentHooks(opts, false); err == nil {
		t.Fatal("duplicate native kata.ts producer accepted")
	}
	if err := os.Remove(sibling); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "authored.js")
	if err := os.WriteFile(target, []byte("authored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "kata.js")); err != nil {
		t.Skip(err)
	}
	if _, err := planPiAgentHooks(opts, true); err == nil {
		t.Fatal("symlink adapter accepted")
	}
	data, _ := os.ReadFile(target) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if string(data) != "authored" {
		t.Fatal("symlink target changed")
	}
}

func TestPiNativeSnapshotsAndAuthoredSettings(t *testing.T) {
	opts := piTestOptions(t)
	opts.SourceSet = true
	opts.Source = "chosen source.txt"
	settings := filepath.Join(opts.Dir, ".pi", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte(`{"extensions":["authored.js"],"packages":["example-package"],"custom":"preserved"}`)
	if err := os.WriteFile(settings, original, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(plan.Path)
	opts.SourceSet = false
	opts.Source = ""
	reinstall, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reinstall.Changes[0].Content, installed) {
		t.Fatal("omitted source changed configured source")
	}
	edited := append(bytes.Clone(installed), []byte("// outside edit\n")...)
	if err = os.WriteFile(plan.Path, edited, 0600); err != nil { //nolint:gosec // G703: planned fixture paths are derived from the isolated test home and SDK setup.
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(reinstall); err == nil {
		t.Fatal("external edit overwritten")
	}
	got, _ := os.ReadFile(plan.Path)
	if !bytes.Equal(got, edited) {
		t.Fatal("external edit lost")
	}
	got, _ = os.ReadFile(settings) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if !bytes.Equal(got, original) {
		t.Fatal("authored settings changed")
	}
}

// An explicit published package root makes CI independent of CLI wrappers.
func piNativeTestRuntime(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("KATA_PI_TEST_RUNTIME"); root != "" {
		if !filepath.IsAbs(root) {
			t.Fatal("KATA_PI_TEST_RUNTIME must be an absolute Pi dist directory")
		}
		if _, err := os.Stat(filepath.Join(root, "core", "extensions", "runner.js")); err != nil { //nolint:gosec // G703: explicitly selected absolute published runtime root for integration tests.
			t.Fatalf("KATA_PI_TEST_RUNTIME does not contain the published Pi runner: %v", err)
		}
		return root
	}
	pi, err := exec.LookPath("pi")
	if err != nil {
		t.Skip("set KATA_PI_TEST_RUNTIME to an isolated published Pi dist directory")
	}
	resolved, err := filepath.EvalSymlinks(pi)
	if err != nil {
		t.Fatal(err)
	}
	// Homebrew wrappers, npm dist/cli.js, and npm dist/bundle/cli.js.
	for _, root := range []string{
		filepath.Join(filepath.Dir(filepath.Dir(resolved)), "libexec", "lib", "node_modules", "@earendil-works", "pi-coding-agent", "dist"),
		filepath.Dir(resolved), filepath.Dir(filepath.Dir(resolved)),
	} {
		if _, err := os.Stat(filepath.Join(root, "core", "extensions", "runner.js")); err == nil {
			return root
		}
	}
	t.Skip("installed Pi runner not discoverable; set KATA_PI_TEST_RUNTIME to its dist directory")
	return ""
}

func TestPiNativeInstalledRunner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	root := piNativeTestRuntime(t)
	opts := piTestOptions(t)
	opts.Attention = true
	opts.SourceSet = true
	opts.Source = "contract '$;`.txt"
	artifactRoot := t.TempDir()
	fake := filepath.Join(artifactRoot, "kata '$;`.mjs")
	executable, err := os.ReadFile("testdata/agent-hooks/pi-executable.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fake, executable, 0700); err != nil { //nolint:gosec // G306: isolated executable recorder must be runnable.
		t.Fatal(err)
	}
	opts.Executable = fake
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(artifactRoot, "project.mjs")
	if err = os.WriteFile(project, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Source = "user.txt"
	plan, err = planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(artifactRoot, "user.mjs")
	if err = os.WriteFile(user, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(artifactRoot, "state.json")
	calls := filepath.Join(artifactRoot, "calls.ndjson")
	command := exec.Command(node, "testdata/agent-hooks/pi-native-runner.mjs", root, project, user, opts.Dir) //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
	command.Env = append(os.Environ(), "KATA_REF=issue-ref", "KATA_INBOX_USER=actor/teammate", "KATA_SERVER=https://daemon.example", "KATA_AUTHOR=actor", "KATA_TEAMMATE=teammate", "KATA_PI_TEST_STATE="+state, "KATA_PI_TEST_CALLS="+calls)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("installed Pi execution: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

func TestPiNativeCapabilityPrecedenceAndReload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	for _, mode := range []string{"contract", "attention"} {
		t.Run(mode, func(t *testing.T) {
			opts := piTestOptions(t)
			opts.Contract = mode == "contract"
			opts.Attention = mode == "attention"
			opts.Executable = "project-kata"
			plan, err := planPiAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			project := filepath.Join(root, "project.mjs")
			if err = os.WriteFile(project, plan.Changes[0].Content, 0600); err != nil {
				t.Fatal(err)
			}
			opts.Scope = "user"
			opts.Contract = true
			opts.Attention = true
			opts.Executable = "user-kata"
			plan, err = planPiAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			user := filepath.Join(root, "user.mjs")
			if err = os.WriteFile(user, plan.Changes[0].Content, 0600); err != nil {
				t.Fatal(err)
			}
			command := exec.Command(node, "testdata/agent-hooks/pi-capabilities.mjs", project, user, opts.Dir, mode) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
			command.Env = append(os.Environ(), "KATA_REF=issue-ref", "KATA_INBOX_USER=")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("Pi scope composition: %v\n%s", err, output)
			}
		})
	}
}

func TestPiNativeRealKataTransport(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	root := piNativeTestRuntime(t)
	opts := piTestOptions(t)
	opts.Attention = true
	opts.SourceSet = true
	opts.Source = "contract '$;`.txt"
	binary := filepath.Join(t.TempDir(), "kata")
	build := exec.Command("go", "build", "-o", binary, ".") //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile Kata: %v\n%s", err, output)
	}
	opts.Executable = binary
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	extension := filepath.Join(t.TempDir(), "kata.mjs")
	if err = os.WriteFile(extension, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(opts.Dir, ".kata.toml"), []byte("version = 1\n[project]\nname = \"example-workspace\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/pi-transport.mjs", root, extension, opts.Dir, filepath.Join(opts.Dir, opts.Source)) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "KATA_") && !strings.HasPrefix(entry, "CLAUDE_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "KATA_HOME="+t.TempDir(), "CLAUDE_PROJECT_DIR="+t.TempDir())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi real Kata transport: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

// Exact generated ownership must survive arbitrary serialized option data;
// this complements the rejecting property rather than weakening edit checks.
func FuzzPiNativeOwnedOptionRoundTrip(f *testing.F) {
	f.Add("kata", "source.txt", true, true, true)
	f.Add("/example path/kata '$;`", "source\nwith quotes \".txt", false, true, false)
	f.Add("kata", "", true, false, true)
	f.Fuzz(func(t *testing.T, executable, source string, user, contract, attention bool) {
		opts := piTestOptions(t)
		opts.Executable = executable
		opts.Source = source
		opts.SourceSet = true
		opts.Contract = contract
		opts.Attention = attention
		if user {
			opts.Scope = "user"
		}
		plan, err := planPiAgentHooks(opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if !contract && !attention {
			if len(plan.Changes) != 0 {
				t.Fatal("no selections produced an adapter")
			}
			return
		}
		if _, err = publishNativeAgentHookPlan(plan); err != nil {
			t.Fatal(err)
		}
		reinstall, err := planPiAgentHooks(opts, false)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := publishNativeAgentHookPlan(reinstall); err != nil || changed {
			t.Fatalf("option roundtrip changed owned artifact: %v %v", changed, err)
		}
	})
}

func TestPiNativeInstalledRuntimeReplacement(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	root := piNativeTestRuntime(t)
	opts := piTestOptions(t)
	opts.Attention = true
	opts.SourceSet = true
	opts.Source = "project.txt"
	artifacts := t.TempDir()
	fake := filepath.Join(artifacts, "kata.mjs")
	executable, err := os.ReadFile("testdata/agent-hooks/pi-executable.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fake, executable, 0700); err != nil { //nolint:gosec // G306: isolated executable recorder must be runnable.
		t.Fatal(err)
	}
	opts.Executable = fake
	plan, err := planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(artifacts, "project.mjs")
	if err = os.WriteFile(project, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Source = "user.txt"
	opts.Executable = filepath.Join(artifacts, "user-kata.mjs")
	if err = os.WriteFile(opts.Executable, executable, 0700); err != nil { //nolint:gosec // G306: isolated executable recorder must be runnable.
		t.Fatal(err)
	}
	plan, err = planPiAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(artifacts, "user.mjs")
	if err = os.WriteFile(user, plan.Changes[0].Content, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/agent-hooks/pi-native-replacement.mjs", root, project, user, opts.Dir) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
	command.Env = append(os.Environ(), "KATA_INBOX_USER=actor/teammate", "KATA_SERVER=https://daemon.example", "KATA_AUTHOR=actor", "KATA_TEAMMATE=teammate", "KATA_PI_TEST_STATE="+filepath.Join(artifacts, "state.json"), "KATA_PI_TEST_CALLS="+filepath.Join(artifacts, "calls.ndjson"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Pi installed runtime replacement: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
}

package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func pluginPlanner(agent string) func(nativeAgentHookOptions, bool) (nativeAgentHookPlan, error) {
	if agent == "amp" {
		return planAmpAgentHooks
	}
	return planOpenCodeAgentHooks
}

func TestNativePluginsPlanning(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, agent := range []string{"amp", "opencode"} {
		for _, scope := range []string{"user", "project"} {
			t.Run(agent+"/"+scope, func(t *testing.T) {
				root := t.TempDir()
				opts := nativeAgentHookOptions{Agent: agent, Scope: scope, Home: root, Dir: root, Contract: true, Executable: "kata", Source: "literal `source`; $(touch bad)", SourceSet: true}
				plan, err := pluginPlanner(agent)(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				if !plan.Contract || plan.AttentionStart || plan.AttentionEnd {
					t.Fatalf("capabilities: %+v", plan)
				}
				if _, err = os.Stat(filepath.Dir(plan.Path)); !os.IsNotExist(err) {
					t.Fatalf("planner mutated disk: %v", err)
				}
				if _, err = publishNativeAgentHookPlan(plan); err != nil {
					t.Fatal(err)
				}
				again, err := pluginPlanner(agent)(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := publishNativeAgentHookPlan(again); err != nil || changed {
					t.Fatalf("reinstall changed=%v err=%v", changed, err)
				}
				opts.Attention = true
				plan, err = pluginPlanner(agent)(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				if !plan.AttentionStart || plan.AttentionEnd {
					t.Fatalf("capabilities: %+v", plan)
				}
				if _, err = publishNativeAgentHookPlan(plan); err != nil {
					t.Fatal(err)
				}
				opts.Attention = false
				opts.SourceSet = false
				opts.Executable = ""
				opts.API = ""
				plan, err = pluginPlanner(agent)(opts, true)
				if err != nil {
					t.Fatal(err)
				}
				if plan.Contract || !plan.AttentionStart || plan.AttentionEnd {
					t.Fatalf("default remove must preserve attention: %+v", plan)
				}
				if _, err = publishNativeAgentHookPlan(plan); err != nil {
					t.Fatal(err)
				}
				opts.Contract = false
				status, err := pluginPlanner(agent)(opts, true)
				if err != nil {
					t.Fatal(err)
				}
				if !status.CurrentAttentionStart || status.CurrentContract {
					t.Fatalf("status: %+v", status)
				}
				if changed, err := publishNativeAgentHookPlan(status); err != nil || changed {
					t.Fatalf("status changed=%v err=%v", changed, err)
				}
				data, err := os.ReadFile(plan.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(plan.Path, append(data, []byte("// authored edit\n")...), 0600); err != nil { //nolint:gosec // G703: planned fixture paths are derived from the isolated test home and SDK setup.
					t.Fatal(err)
				}
				if _, err = pluginPlanner(agent)(opts, true); err == nil {
					t.Fatal("edited artifact accepted")
				}
			})
		}
	}
}

func TestNativePluginsOpenCodeAPIAndConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	cfg := filepath.Join(root, "opencode.jsonc")
	authored := []byte("{\n // keep policy\n \"plugins\": [\"foreign-plugin\", {\"package\":\"other\",\"options\":{\"x\":1}},],\n \"permissions\": [{\"effect\":\"deny\"}],\n}\n")
	if err := os.WriteFile(cfg, authored, 0600); err != nil {
		t.Fatal(err)
	}
	opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, ConfigPath: cfg, Contract: true}
	plan, err := planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{plan.Path, filepath.Join(filepath.Dir(plan.Path), "package.json")} {
		if _, err = os.Stat(file); err != nil {
			t.Fatalf("native package bundle not published: %v", err)
		}
	}
	installed, err := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installed, []byte("// keep policy")) || !bytes.Contains(installed, []byte("{\"package\":\"other\",\"options\":{\"x\":1}}")) {
		t.Fatalf("foreign bytes lost: %s", installed)
	}
	opts.API = "v1"
	if _, err = planOpenCodeAgentHooks(opts, false); err == nil {
		t.Fatal("incompatible API replacement accepted")
	}
	opts.API = ""
	plan, err = planOpenCodeAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, authored) {
		t.Fatalf("foreign config changed:\n%s", restored)
	}
	for _, bad := range []string{`{"plugins":false}`, `{"plugins":[],"plugins":[]}`, `{"plugins":[broken]}`} {
		if err = os.WriteFile(cfg, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = planOpenCodeAgentHooks(opts, false); err == nil {
			t.Fatalf("invalid config accepted: %s", bad)
		}
	}
}

func TestNativePluginsRuntime(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	if runtime.GOOS == "windows" {
		t.Skip("Node executable fixture uses Unix shebang; native Windows execFile runs the installed kata.exe")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, api := range []string{"amp", "v1", "v2"} {
		modes := []string{"both", "project-contract", "project-attention", "project-unloaded"}
		if api == "v2" {
			modes = append(modes, "project-pending-context")
		}
		if api == "amp" {
			modes = []string{"single-user", "single-project", "single-user-contract-retry"}
		}
		for _, mode := range modes {
			t.Run(api+"/"+mode, func(t *testing.T) {
				root := t.TempDir()
				workspace := filepath.Join(root, "native-workspace ")
				otherWorkspace := filepath.Join(root, "other-workspace")
				for _, dir := range []string{workspace, otherWorkspace} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				fake := filepath.Join(root, "kata executable.js")
				fakeJS := `#!/usr/bin/env node
const fs=require('node:fs'); const a=process.argv.slice(2);fs.appendFileSync(process.env.KATA_PLUGIN_LOG,JSON.stringify({args:a,cwd:process.cwd(),ref:process.env.KATA_REF,server:process.env.KATA_SERVER})+'\n');
if(a.includes('agent-contract-hook')){if(process.env.KATA_PLUGIN_CONTRACT_DELAY==='1')Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,75);const n=Number(fs.readFileSync(process.env.KATA_PLUGIN_CONTRACT_ATTEMPTS,'utf8'));fs.writeFileSync(process.env.KATA_PLUGIN_CONTRACT_ATTEMPTS,String(n+1));if(process.env.KATA_PLUGIN_CONTRACT_FAIL_FIRST==='1'&&n===0)process.exit(1);console.log(JSON.stringify({hookSpecificOutput:{additionalContext:'contract:'+a.at(-1)}}));}
else if(a.includes('inbox')){let s=fs.readFileSync(process.env.KATA_PLUGIN_STATE,'utf8');if(s==='fail')process.exit(1);process.stdout.write(s);}
`
				if err = os.WriteFile(fake, []byte(fakeJS), 0700); err != nil { //nolint:gosec // G306: executable permission is required for the native launcher fixture.
					t.Fatal(err)
				}
				agent := "opencode"
				if api == "amp" {
					agent = "amp"
				}
				opts := nativeAgentHookOptions{Agent: agent, Home: root, Dir: workspace, Scope: "user", Executable: fake, Contract: true, Attention: true, Source: "source $(touch bad) `echo bad`; spaced", SourceSet: true}
				if api != "amp" {
					opts.API = api
				} else if mode == "single-project" {
					opts.Scope = "project"
				}
				plan, err := pluginPlanner(agent)(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = publishNativeAgentHookPlan(plan); err != nil {
					t.Fatal(err)
				}
				opts.Scope = "project"
				opts.Contract = mode != "project-attention"
				opts.Attention = mode != "project-contract"
				project := plan
				if api != "amp" {
					project, err = pluginPlanner(agent)(opts, false)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = publishNativeAgentHookPlan(project); err != nil {
						t.Fatal(err)
					}
				}
				log := filepath.Join(root, "calls.jsonl")
				state := filepath.Join(root, "inbox.txt")
				contractAttempts := filepath.Join(root, "contract-attempts.txt")
				if err = os.WriteFile(state, []byte("fresh request"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(contractAttempts, []byte("0"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(node, "testdata/agent-hooks/plugins-native.mjs", api, plan.Path, project.Path, workspace, otherWorkspace, mode) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
				cmd.Env = append(os.Environ(), "KATA_PLUGIN_LOG="+log, "KATA_PLUGIN_STATE="+state, "KATA_REF=spoke-project#abc4", "KATA_INBOX_USER=actor/worker", "KATA_SERVER=http://127.0.0.1:7777")
				cmd.Env = append(cmd.Env, "KATA_PLUGIN_CONTRACT_ATTEMPTS="+contractAttempts)
				if api == "amp" && mode == "single-user-contract-retry" {
					cmd.Env = append(cmd.Env, "KATA_PLUGIN_CONTRACT_FAIL_FIRST=1", "KATA_PLUGIN_CONTRACT_DELAY=1")
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("runtime: %v\n%s", err, out)
				}
				if !strings.Contains(string(out), "native plugin fixture passed") {
					t.Fatalf("fixture did not complete: %s", out)
				}
			})
		}
	}
}

func TestNativePluginsConfigPreservesArbitraryForeignStrings(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		foreign := rapid.SliceOf(rapid.String()).Draw(rt, "foreign plugins")
		entries, err := json.Marshal(foreign)
		if err != nil {
			rt.Fatal(err)
		}
		if foreign == nil {
			entries = []byte("[]")
		}
		original := append([]byte("{\n// authored\n\"plugins\":"), entries...)
		original = append(original, []byte(",\"other\":{\"enabled\":false},\n}\n")...)
		installed, present, _, err := editPluginAgentHookConfig(original, "file:///example/kata-plugin", true, false)
		if err != nil {
			rt.Fatal(err)
		}
		if present {
			rt.Skip("authored entry already matches owned URL")
		}
		restored, _, _, err := editPluginAgentHookConfig(installed, "file:///example/kata-plugin", false, true)
		if err != nil {
			rt.Fatal(err)
		}
		if !bytes.Equal(restored, original) {
			rt.Fatalf("foreign bytes changed: before=%q after=%q", original, restored)
		}
	})
}

func TestNativePluginsRemoveReorderedOwnedConfigProperty(t *testing.T) {
	// An operator may reorder foreign fields after installation. Removing an
	// owned empty plugins property must leave valid JSON and preserve those fields.
	original := []byte(`{"foreign": true,"plugins":[]}`)
	got, err := removeEmptyPluginAgentHookProperty(original)
	if err != nil {
		t.Fatal(err)
	}
	if !jsontext.Value(got).IsValid() {
		t.Fatalf("invalid config after removal: %s", got)
	}
	if !bytes.Contains(got, []byte(`"foreign": true`)) {
		t.Fatalf("foreign bytes lost: %s", got)
	}
}

func TestNativePluginsValidation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, agent := range []string{"amp", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			root := t.TempDir()
			opts := nativeAgentHookOptions{Agent: agent, Home: root, Dir: root, Contract: false}
			absent, err := pluginPlanner(agent)(opts, true)
			if err != nil || len(absent.Changes) != 0 {
				t.Fatalf("absent status: %+v %v", absent, err)
			}
			opts.Contract = true
			if agent == "opencode" {
				opts.API = "v1"
			}
			plan, err := pluginPlanner(agent)(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(plan); err != nil {
				t.Fatal(err)
			}
			opts.API = ""
			opts.Executable = ""
			opts.Contract = false
			inspected, err := pluginPlanner(agent)(opts, true)
			if err != nil {
				t.Fatal(err)
			}
			if !inspected.CurrentContract || inspected.CurrentAttentionStart || inspected.CurrentAttentionEnd {
				t.Fatalf("status: %+v", inspected)
			}
			if changed, err := publishNativeAgentHookPlan(inspected); err != nil || changed {
				t.Fatalf("status published mutation: %v %v", changed, err)
			}
			opts.Contract = true
			again, err := pluginPlanner(agent)(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if again.Path != plan.Path {
				t.Fatalf("omitted API changed path: %s %s", plan.Path, again.Path)
			}
			if err = os.WriteFile(plan.Path, []byte("export default ()=>{};\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = pluginPlanner(agent)(opts, false); err == nil {
				t.Fatal("authored plugin overwritten")
			}
			opts.Scope = "invalid"
			if _, err = pluginPlanner(agent)(opts, false); err == nil {
				t.Fatal("invalid scope accepted")
			}
		})
	}
}

func TestNativePluginsRejectAuthoredPackage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, Contract: true}
	plan, err := planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(filepath.Dir(plan.Path), "package.json")
	if err = os.WriteFile(manifest, []byte(`{"name":"authored"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = planOpenCodeAgentHooks(opts, true); err == nil {
		t.Fatal("edited package accepted during removal")
	}
	data, err := os.ReadFile(plan.Path)
	if err != nil || len(data) == 0 {
		t.Fatal("planning removal mutated plugin")
	}
}

func TestNativePluginsAmpRejectsConflictingScopes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, first := range []string{"user", "project"} {
		t.Run(first, func(t *testing.T) {
			root := t.TempDir()
			opts := nativeAgentHookOptions{Agent: "amp", Home: root, Dir: root, Scope: first, Contract: true, Attention: true}
			plan, err := planAmpAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(plan); err != nil {
				t.Fatal(err)
			}
			if first == "user" {
				opts.Scope = "project"
			} else {
				opts.Scope = "user"
			}
			if _, err = planAmpAgentHooks(opts, false); err == nil {
				t.Fatal("cross-process Amp scopes accepted without native per-capability precedence")
			}
			opts.Scope = first
			opts.Contract = false
			opts.Attention = false
			status, err := planAmpAgentHooks(opts, true)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := publishNativeAgentHookPlan(status); err != nil || changed {
				t.Fatalf("inspection changed state: %v %v", changed, err)
			}
		})
	}
}

func TestNativePluginsAmpScopeIndex(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	home := filepath.Join(root, "home")
	first := filepath.Join(root, "first-workspace")
	second := filepath.Join(root, "second-workspace")
	opts := nativeAgentHookOptions{Agent: "amp", Home: home, Dir: first, Scope: "project", Contract: true, Attention: true}
	firstPlan, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(firstPlan); err != nil {
		t.Fatal(err)
	}
	opts.Dir = second
	secondPlan, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(secondPlan); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Dir = root
	if _, err = planAmpAgentHooks(opts, false); err == nil {
		t.Fatal("user install from unrelated cwd missed owned project plugins")
	}
	// Edited tracked artifacts cannot silently become stale index entries.
	data, err := os.ReadFile(firstPlan.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(firstPlan.Path, append(data, []byte("// edit\n")...), 0600); err != nil { //nolint:gosec // G703: planned fixture paths are derived from the isolated test home and SDK setup.
		t.Fatal(err)
	}
	if _, err = planAmpAgentHooks(opts, false); err == nil {
		t.Fatal("edited tracked project was ignored")
	}
	// Missing files reconcile without scanning or deleting any authored file.
	if err = os.Remove(firstPlan.Path); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(secondPlan.Path); err != nil {
		t.Fatal(err)
	}
	userPlan, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(userPlan); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "project"
	opts.Dir = second
	if _, err = planAmpAgentHooks(opts, false); err == nil {
		t.Fatal("project install missed owned user plugin")
	}
}

func TestNativePluginsAmpScopeIndexRetainsAttentionOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "amp", Home: root, Dir: filepath.Join(root, "workspace"), Scope: "project", Contract: true, Attention: true}
	plan, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	opts.Attention = false
	plan, err = planAmpAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Dir = root
	if _, err = planAmpAgentHooks(opts, false); err == nil {
		t.Fatal("attention-only project lost its scope record")
	}
	opts.Scope = "project"
	opts.Dir = filepath.Join(root, "workspace")
	opts.Attention = true
	plan, err = planAmpAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	opts.Scope = "user"
	opts.Dir = root
	if _, err = planAmpAgentHooks(opts, false); err != nil {
		t.Fatalf("full removal did not release scope index: %v", err)
	}
}

func TestNativePluginsAmpScopeIndexFencesConcurrentProjectInstall(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "amp", Home: root, Dir: filepath.Join(root, "project-workspace"), Scope: "user", Contract: true}
	pendingUser, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	opts.Scope = "project"
	project, err := planAmpAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(project); err != nil {
		t.Fatal(err)
	}
	if changed, err := publishNativeAgentHookPlan(pendingUser); err == nil || changed {
		t.Fatalf("concurrent project install bypassed scope index: changed=%v err=%v", changed, err)
	}
	if _, err = os.Stat(pendingUser.Path); !os.IsNotExist(err) {
		t.Fatalf("rejected user install published an artifact: %v", err)
	}
}

func TestNativePluginsPublishedDeclarations(t *testing.T) {
	sdkRoot := os.Getenv("KATA_NATIVE_PLUGIN_SDK_ROOT")
	if sdkRoot == "" {
		t.Skip("set KATA_NATIVE_PLUGIN_SDK_ROOT to an isolated published SDK install")
	}
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(sdkRoot, "node_modules"), filepath.Join(root, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, api := range []string{"amp", "v1", "v2"} {
		agent := "opencode"
		if api == "amp" {
			agent = "amp"
		}
		meta := pluginAgentHookMetadata{Format: "kata-plugin", Version: 1, Agent: agent, API: api, Scope: "user", Executable: "kata", Contract: true, Attention: true}
		data, err := generatePluginAgentHooks(meta)
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		// Add only erased boundary annotations to the unchanged emitted JS. The
		// published declarations then check the actual registered callback bodies.
		if api == "amp" {
			source = strings.Replace(source, "export default function kataHooks(amp)", `/** @param {import("@ampcode/plugin").PluginAPI} amp */`+"\nexport default function kataHooks(amp)", 1)
			source = strings.Replace(source, "async function workspace(ctx)", `/** @param {import("@ampcode/plugin").PluginEventContext<"agent.start">} ctx */`+"\nasync function workspace(ctx)", 1)
		}
		if api == "v1" {
			source = strings.Replace(source, "export const KataPlugin =", `/** @type {import("@opencode-ai/plugin").Plugin} */`+"\nexport const KataPlugin =", 1)
			source = strings.Replace(source, "const hooks = {};", `/** @type {import("@opencode-ai/plugin").Hooks} */`+"\nconst hooks = {};", 1)
		}
		file := filepath.Join(root, api+".js")
		if err = os.WriteFile(file, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	args := append([]string{"--allowJs", "--checkJs", "--noEmit", "--skipLibCheck", "--module", "nodenext", "--target", "es2022", "--types", "node", "--noImplicitAny", "false", "--strictNullChecks", "true"}, files...)
	cmd := exec.Command(filepath.Join(sdkRoot, "node_modules", ".bin", "tsc"), args...) //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("published declarations rejected generated callbacks: %v\n%s", err, output)
	}
}

func TestNativePluginsRejectShadowingTypeScript(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, api := range []string{"amp", "v1", "v2"} {
		t.Run(api, func(t *testing.T) {
			root := t.TempDir()
			agent := "opencode"
			if api == "amp" {
				agent = "amp"
			}
			opts := nativeAgentHookOptions{Agent: agent, API: api, Home: root, Dir: root, Contract: true}
			if api == "amp" {
				opts.API = ""
			}
			plan, err := pluginPlanner(agent)(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			sibling := strings.TrimSuffix(plan.Path, ".js") + ".ts"
			if err = os.MkdirAll(filepath.Dir(sibling), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(sibling, []byte("export default ()=>{};\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = pluginPlanner(agent)(opts, false); err == nil {
				t.Fatal("authored TypeScript counterpart accepted")
			}
		})
	}
}

func TestNativePluginsOpenCodeV1NativeDiscovery(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	for _, scope := range []string{"user", "project"} {
		opts := nativeAgentHookOptions{Agent: "opencode", API: "v1", Home: root, Dir: root, Scope: scope, Contract: true}
		plan, err := planOpenCodeAgentHooks(opts, false)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(root, ".config", "opencode", "plugin", "kata-user.js")
		if scope == "project" {
			want = filepath.Join(root, ".opencode", "plugin", "kata-project.js")
		}
		if plan.Path != want {
			t.Fatalf("OpenCode 1.0.154 discovery path: got %q want %q", plan.Path, want)
		}
	}
}

func TestNativePluginsFileURLNativeParser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	for _, tc := range []struct {
		name, directory, want string
		windows               bool
	}{
		{"UNC", "//server.example/share/plugin space/#entry", `\\server.example\share\plugin space\#entry`, true},
		{"drive", "C:/Users/example/plugin space/#entry", `C:\Users\example\plugin space\#entry`, true},
		{"unix", "/home/example/plugin space/#entry", "/home/example/plugin space/#entry", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registration := nativePluginFileURL(tc.directory)
			windows := "false"
			if tc.windows {
				windows = "true"
			}
			cmd := exec.Command(node, "--input-type=module", "-e", `import {fileURLToPath} from 'node:url'; process.stdout.write(fileURLToPath(process.argv[1], {windows:process.argv[2]==='true'}));`, registration, windows) //nolint:gosec // G204: controlled native fixture executable and arguments exercise the generated integration.
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("native file URL parser rejected %q: %v\n%s", registration, err, out)
			}
			if string(out) != tc.want {
				t.Fatalf("native URL path got %q want %q", out, tc.want)
			}
		})
	}
}

func TestNativePluginsOpenCodeMissingEntrypointOwnership(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, authoredRegistration := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-registration", true: "authored-registration"}[authoredRegistration], func(t *testing.T) {
			root := t.TempDir()
			cfg := filepath.Join(root, "opencode.jsonc")
			opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, ConfigPath: cfg, Contract: true}
			initial, err := planOpenCodeAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			registration := nativePluginFileURL(filepath.ToSlash(filepath.Dir(initial.Path)))
			original := []byte("{\n // authored policy\n \"permissions\": [\"deny\"],\n}\n")
			if authoredRegistration {
				encoded, _ := json.Marshal(registration)
				original = []byte("{\n // authored policy\n \"plugins\": [" + string(encoded) + "],\n \"permissions\": [\"deny\"],\n}\n")
			}
			if err = os.WriteFile(cfg, original, 0600); err != nil {
				t.Fatal(err)
			}
			apply := func(remove bool) nativeAgentHookPlan {
				t.Helper()
				p, e := planOpenCodeAgentHooks(opts, remove)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = publishNativeAgentHookPlan(p); e != nil {
					t.Fatal(e)
				}
				return p
			}
			installed := apply(false)
			if err = os.Remove(installed.Path); err != nil {
				t.Fatal(err)
			}
			statusOpts := opts
			statusOpts.Contract = false
			statusOpts.Executable = ""
			status, err := planOpenCodeAgentHooks(statusOpts, true)
			if err != nil {
				t.Fatal(err)
			}
			if status.CurrentContract {
				t.Fatal("missing entrypoint reported configured")
			}
			if changed, e := publishNativeAgentHookPlan(status); e != nil || changed {
				t.Fatalf("status mutated recovery assets: %v %v", changed, e)
			}
			apply(true)
			restored, err := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored, original) {
				t.Fatalf("missing entrypoint uninstall lost ownership or foreign config:\n%s", restored)
			}
			if _, err = os.Stat(filepath.Join(filepath.Dir(installed.Path), "package.json")); !os.IsNotExist(err) {
				t.Fatalf("owned recovery package retained: %v", err)
			}
			apply(false)
			opts.Attention = true
			apply(true)
			restored, err = os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil || !bytes.Equal(restored, original) {
				t.Fatalf("reinstall/full uninstall left registration: %s %v", restored, err)
			}
		})
	}
}

func TestNativePluginsOpenCodeReinstallMissingEntrypoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, Contract: true, Attention: true, Source: "preserved source", SourceSet: true}
	plan, err := planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(plan.Path); err != nil {
		t.Fatal(err)
	}
	opts.Contract = false
	opts.Attention = false
	opts.SourceSet = false
	plan, err = planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Contract || !plan.AttentionStart {
		t.Fatalf("missing entrypoint lost capabilities: %+v", plan)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plan.Path)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := parsePluginAgentHookMetadata(data)
	if err != nil || !meta.ConfigOwned || meta.Source != "preserved source" {
		t.Fatalf("recovery lost metadata: %+v %v", meta, err)
	}
	opts.Contract = true
	opts.Attention = true
	plan, err = planOpenCodeAgentHooks(opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(plan); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".config", "opencode", "opencode.json")
	data, err = os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	if err != nil || bytes.Contains(data, []byte("plugins")) {
		t.Fatalf("owned registration/property left behind: %s %v", data, err)
	}
}

func TestNativePluginsOpenCodeMissingEntrypointAttentionAndLegacy(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "recoverable", true: "legacy"}[legacy], func(t *testing.T) {
			root := t.TempDir()
			opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, Contract: true, Attention: true}
			installed, err := planOpenCodeAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(installed); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(installed.Path); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(filepath.Dir(installed.Path), "package.json")
			if legacy {
				if err = os.WriteFile(manifest, pluginAgentHookPackage, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := filepath.Join(root, ".config", "opencode", "opencode.json")
			before, err := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil {
				t.Fatal(err)
			}
			opts.Attention = false
			removal, err := planOpenCodeAgentHooks(opts, true)
			if err != nil {
				t.Fatal(err)
			}
			if removal.CurrentContract || removal.CurrentAttentionStart {
				t.Fatal("missing entrypoint claimed configured capability")
			}
			if legacy {
				if !strings.Contains(strings.Join(removal.Warnings, " "), "cannot prove config registration ownership") {
					t.Fatal("legacy incomplete registration lacks warning")
				}
				if changed, e := publishNativeAgentHookPlan(removal); e != nil || changed {
					t.Fatalf("legacy ownership guessed: %v %v", changed, e)
				}
				after, e := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
				if e != nil || !bytes.Equal(before, after) {
					t.Fatal("legacy config changed")
				}
				return
			}
			if removal.Contract || !removal.AttentionStart {
				t.Fatal("default missing-entry removal lost independent attention")
			}
			if _, err = publishNativeAgentHookPlan(removal); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(manifest) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil {
				t.Fatal(err)
			}
			meta, err := parsePluginAgentHookPackage(data)
			if err != nil || meta.Contract || !meta.Attention || !meta.ConfigOwned {
				t.Fatalf("retained metadata: %+v %v", meta, err)
			}
			opts.Attention = true
			removal, err = planOpenCodeAgentHooks(opts, true)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(manifest, append(bytes.Clone(data), []byte(" ")...), 0600); err != nil { //nolint:gosec // G703: planned fixture paths are derived from the isolated test home and SDK setup.
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(removal); err == nil {
				t.Fatal("recovery manifest changed after planning but publish proceeded")
			}
			if err = os.WriteFile(manifest, data, 0600); err != nil { //nolint:gosec // G703: planned fixture paths are derived from the isolated test home and SDK setup.
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(removal); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(cfg) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			if err != nil || bytes.Contains(after, []byte("plugins")) {
				t.Fatalf("full missing-entry removal stranded registration: %s %v", after, err)
			}
		})
	}
}

func TestNativePluginsOpenCodeUnchangedConfigPreimage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	opts := nativeAgentHookOptions{Agent: "opencode", Home: root, Dir: root, Contract: true}
	installed, err := planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(installed); err != nil {
		t.Fatal(err)
	}
	opts.Attention = true
	upgrade, err := planOpenCodeAgentHooks(opts, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, ".config", "opencode", "opencode.json")
	if err = os.WriteFile(cfg, []byte("{\"plugins\":[],\"authored\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = publishNativeAgentHookPlan(upgrade); err == nil {
		t.Fatal("publication accepted config registration removed after planning")
	}
	current, err := os.ReadFile(installed.Path)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := parsePluginAgentHookMetadata(current)
	if err != nil || meta.Attention {
		t.Fatalf("preimage rejection allowed partial upgrade: %+v %v", meta, err)
	}
}

func TestNativePluginsDiscoveryPreimages(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, api := range []string{"amp", "v1", "v2"} {
		for _, collision := range []string{"alternate", "typescript"} {
			t.Run(api+"/"+collision, func(t *testing.T) {
				root := t.TempDir()
				agent := "opencode"
				selectedAPI := api
				if api == "amp" {
					agent = "amp"
					selectedAPI = ""
				}
				opts := nativeAgentHookOptions{Agent: agent, API: selectedAPI, Home: root, Dir: root, Contract: true}
				plan, err := pluginPlanner(agent)(opts, false)
				if err != nil {
					t.Fatal(err)
				}
				path := strings.TrimSuffix(plan.Path, ".js") + ".ts"
				if collision == "alternate" {
					path = filepath.Join(root, ".config", agent, "plugins", "kata-user", "index.js")
					if api == "v2" {
						path = filepath.Join(root, ".config", agent, "plugin", "kata-user.js")
					}
				}
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, []byte("export default ()=>{};\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = publishNativeAgentHookPlan(plan); err == nil {
					t.Fatal("native discovery precondition changed but publication proceeded")
				}
				if _, err = os.Stat(plan.Path); !os.IsNotExist(err) {
					t.Fatalf("failed publish created managed entrypoint: %v", err)
				}
			})
		}
	}
}

func TestNativePluginsAbsentGuardsDoNotCreateDiscoveryDirectories(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	for _, api := range []string{"v1", "v2"} {
		t.Run(api, func(t *testing.T) {
			root := t.TempDir()
			opts := nativeAgentHookOptions{Agent: "opencode", API: api, Home: root, Dir: root, Contract: true}
			plan, err := planOpenCodeAgentHooks(opts, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = publishNativeAgentHookPlan(plan); err != nil {
				t.Fatal(err)
			}
			forbidden := "plugins"
			if api == "v2" {
				forbidden = "plugin"
			}
			if _, err = os.Stat(filepath.Join(root, ".config", "opencode", forbidden)); !os.IsNotExist(err) {
				t.Fatalf("absent guard created unsupported %s discovery directory: %v", forbidden, err)
			}
		})
	}
}

func TestNativePluginsJSONCRejectsNestedDuplicates(t *testing.T) {
	for _, input := range []string{`{"plugins":[],"foreign":{"same":1,"same":2}}`, `{"plugins":[{"options":{"same":1,"same":2}}]}`} {
		if _, _, _, err := editPluginAgentHookConfig([]byte(input), "file:///example/kata", true, false); err == nil {
			t.Fatalf("ambiguous nested object accepted: %s", input)
		}
	}
}

func TestNativePluginsJSONCRetainsLargeNumbersAndOffsets(t *testing.T) {
	input := []byte("{\n // preserve arithmetic\n \"foreign\": {\"large\": 9007199254740993123456789, \"exponent\": 1e400},\n \"plugins\": [\"foreign-plugin\",],\n}\n")
	installed, _, _, err := editPluginAgentHookConfig(input, "file:///example/kata", true, false)
	if err != nil {
		t.Fatal(err)
	}
	restored, _, _, err := editPluginAgentHookConfig(installed, "file:///example/kata", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, restored) {
		t.Fatalf("numeric bytes/JSONC offsets changed:\n%s", restored)
	}
	for _, invalid := range []string{`{"plugins":[]} {}`, `{"plugins":[]} true`} {
		if _, _, _, err = editPluginAgentHookConfig([]byte(invalid), "file:///example/kata", true, false); err == nil {
			t.Fatalf("trailing JSON accepted: %s", invalid)
		}
	}
}

func TestNativePluginsLegacyJSONMetadataBytes(t *testing.T) {
	meta := pluginAgentHookMetadata{Format: "kata-plugin", Version: 1, Agent: "amp", API: "amp", Scope: "user", Executable: "kata", Source: "<>&\u2028\u2029", SourceSet: true, Contract: true}
	legacy := `{"format":"kata-plugin","version":1,"agent":"amp","api":"amp","scope":"user","workspace":"","executable":"kata","source":"\u003c\u003e\u0026\u2028\u2029","sourceSet":true,"contract":true,"attention":false,"configPath":"","configOwned":false,"configProperty":false}`
	artifact := []byte(pluginAgentHookMetadataPrefix + legacy + "\nconst options = " + legacy + ";\n" + pluginAgentHookCommonJS + ampAgentHookJS)
	generated, err := generatePluginAgentHooks(meta)
	if err != nil || !bytes.Equal(generated, artifact) {
		t.Fatalf("legacy generated bytes changed: %v", err)
	}
	parsed, err := parsePluginAgentHookMetadata(artifact)
	if err != nil || parsed != meta {
		t.Fatalf("legacy owned extension rejected: %+v %v", parsed, err)
	}
	meta.Agent = "opencode"
	meta.API = "v2"
	manifest := generatePluginAgentHookPackage(meta)
	if !bytes.Contains(manifest, []byte(`"source":"\u003c\u003e\u0026\u2028\u2029"`)) {
		t.Fatalf("legacy manifest encoding changed: %s", manifest)
	}
	parsed, err = parsePluginAgentHookPackage(manifest)
	if err != nil || parsed != meta {
		t.Fatalf("owned package metadata rejected: %+v %v", parsed, err)
	}
}

func TestNativePluginsLegacyNullScopeIndex(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	root := t.TempDir()
	index := filepath.Join(root, ".config", "amp", "kata-agent-hook-scopes.json")
	if err := os.MkdirAll(filepath.Dir(index), 0700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte("{\n  \"format\": \"kata-amp-scopes\",\n  \"version\": 1,\n  \"projects\": null\n}\n")
	if err := os.WriteFile(index, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	opts := nativeAgentHookOptions{Home: root, Dir: root, Scope: "user"}
	meta := pluginAgentHookMetadata{Format: "kata-plugin", Version: 1, Agent: "amp", API: "amp", Scope: "user", Executable: "kata"}
	if _, _, err := planAmpAgentHookScopeIndex(opts, meta, filepath.Join(filepath.Dir(index), "plugins", "kata-user.js"), true); err != nil {
		t.Fatalf("legacy null index rejected: %v", err)
	}
	retained, err := os.ReadFile(index) //nolint:gosec // G304: index is a planned artifact inside this test's isolated config home.
	if err != nil || !bytes.Equal(retained, legacy) {
		t.Fatalf("legacy index inspection mutated bytes: %s %v", retained, err)
	}
}

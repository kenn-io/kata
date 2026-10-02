package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"gopkg.in/yaml.v3"
)

func TestAgentHooksExactOwnershipInspection(t *testing.T) {
	for _, tc := range []struct {
		name, command, alternate string
		owned                    bool
	}{
		{"bare", "kata agent-contract-hook", "", true},
		{"native", "kata agent-hooks contract codex", "", true},
		{"old", "kata agent-hooks contract codex --source kata-agent-contract-hook", "", true},
		{"custom", "kata agent-contract-hook --source prompt.txt", "", false},
		{"wrapper", "notify kata agent-hooks contract codex --source kata-agent-contract-hook", "", false},
		{"pipeline", "kata agent-contract-hook | cat", "", false},
		{"environment", "MODE=1 kata agent-contract-hook", "", false},
		{"wrong harness", "kata agent-hooks contract claude", "", false},
		{"custom Windows", "kata agent-contract-hook", "kata agent-contract-hook --source prompt.txt", false},
		{"wrapped Windows", "kata agent-hooks contract codex --source kata-agent-contract-hook", "notify kata agent-contract-hook", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := map[string]any{"type": "command", "command": tc.command}
			if tc.alternate != "" {
				handler["commandWindows"] = tc.alternate
			}
			data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{handler}}}}})
			require.NoError(t, err)
			entries, err := parseAgentHookEntries(agenthook.AgentCodex, data)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, tc.owned, entries[0].Contract)
		})
	}
}

func TestAgentHooksWorkspaceBareAndStable(t *testing.T) {
	isolateAgentHookHomes(t)
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			dir := t.TempDir()
			var changed bool
			var err error
			apply := func() {
				if harness == "claude" {
					changed, err = applyClaudeHooks(dir)
				} else {
					changed, _, err = applyCodexHooks(dir)
				}
			}
			apply()
			require.NoError(t, err)
			require.True(t, changed)
			path := filepath.Join(dir, ".claude", "settings.json")
			agent := agenthook.AgentClaude
			if harness == "codex" {
				path = filepath.Join(dir, ".codex", "hooks.json")
				agent = agenthook.AgentCodex
			}
			before, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
			require.NoError(t, err)
			entries, err := parseAgentHookEntries(agent, before)
			require.NoError(t, err)
			for _, entry := range entries {
				if entry.Event == "SessionEnd" {
					require.Equal(t, "kata attention-hook end", entry.Command)
				} else if entry.Contract {
					require.Equal(t, "kata agent-contract-hook", entry.Command)
				} else {
					require.Equal(t, "kata attention-hook start", entry.Command)
				}
			}
			apply()
			require.NoError(t, err)
			require.False(t, changed)
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestAgentHooksInstallAndUninstallProtectForeignVariants(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	foreign := []map[string]any{
		{"type": "command", "command": "kata agent-contract-hook --source prompt.txt"},
		{"type": "command", "command": "notify kata agent-hooks contract codex --source kata-agent-contract-hook"},
		{"type": "command", "command": "kata agent-hooks contract codex --source kata-agent-contract-hook", "commandWindows": "kata agent-contract-hook --source windows.txt"},
		{"type": "command", "command": "kata agent-contract-hook", "args": []any{"--source", "extra.txt"}},
	}
	path := filepath.Join(t.TempDir(), "hooks.json")
	raw, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": foreign}}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o640)) //nolint:gosec // G306: deliberately verify preservation of non-default fixture permissions.
	beforeInfo, err := os.Stat(path)
	require.NoError(t, err)
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	handlers := nativeContractHandlers(t, agenthook.AgentCodex, path)
	require.Len(t, handlers, 5)
	for i, expected := range foreign {
		delete(handlers[i], "matcher")
		require.Equal(t, expected, handlers[i])
	}
	require.NotContains(t, handlers[4]["command"], "--source")
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "uninstall", "codex", "--config", path)
	require.NoError(t, err, stderr)
	handlers = nativeContractHandlers(t, agenthook.AgentCodex, path)
	require.Len(t, handlers, 4)
	for i, expected := range foreign {
		delete(handlers[i], "matcher")
		require.Equal(t, expected, handlers[i])
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, beforeInfo.Mode().Perm(), info.Mode().Perm())
}

func TestAgentHooksMutationUsesCapturedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	snapshot := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo foreign"},{"type":"command","command":"kata agent-contract-hook"}]}]}}`)
	// Another installer replaced the file after this operation captured its bytes.
	require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600))
	result, err := planOwnedAgentHookSnapshot(agenthook.AgentCodex, path, snapshot, true, contractHook, nil)
	require.NoError(t, err)
	require.True(t, result.result.Changed)
	entries, err := parseAgentHookEntries(agenthook.AgentCodex, result.result.Data)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "echo foreign", entries[0].Command)
}

func TestAgentHooksMutationRefusesToOverwriteConfigChangedAfterPlanning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	snapshot := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo foreign"},{"type":"command","command":"kata agent-contract-hook"}]}]}}`)
	require.NoError(t, os.WriteFile(path, snapshot, 0o600))
	plan, err := planOwnedAgentHookSnapshot(agenthook.AgentCodex, path, snapshot, true, contractHook, nil)
	require.NoError(t, err)
	require.True(t, plan.result.Changed)

	// Preserve another writer's edit between snapshot planning and publication.
	concurrent := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo concurrent"}]}]}}`)
	require.NoError(t, os.WriteFile(path, concurrent, 0o600))
	_, err = writeOwnedAgentHookPlan(plan)
	assert.ErrorContains(t, err, "changed after planning")

	after, readErr := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, concurrent, after)
}

func TestAgentHooksMutationRechecksAfterReplacementIsStaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	snapshot := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo foreign"},{"type":"command","command":"kata agent-contract-hook"}]}]}}`)
	require.NoError(t, os.WriteFile(path, snapshot, 0o600))
	plan, err := planOwnedAgentHookSnapshot(agenthook.AgentCodex, path, snapshot, true, contractHook, nil)
	require.NoError(t, err)

	staged, err := stageOwnedAgentHookPlan(plan)
	require.NoError(t, err)
	defer func() { _ = staged.Abort() }()

	// A second writer edits the config while the replacement is being staged.
	concurrent := []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo concurrent"}]}]}}`)
	require.NoError(t, os.WriteFile(path, concurrent, 0o600))
	_, err = publishOwnedAgentHookPlan(plan, staged)
	assert.ErrorContains(t, err, "changed after planning")

	after, readErr := os.ReadFile(path) //nolint:gosec // test-owned config under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, concurrent, after)
}

func TestAgentHooksMutationValidatesCapturedSnapshot(t *testing.T) {
	for _, install := range []bool{false, true} {
		t.Run(map[bool]string{false: "uninstall", true: "install"}[install], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hooks.json")
			require.NoError(t, os.WriteFile(path, []byte(`{"hooks":{}}`), 0o600))
			var opts *agenthook.InstallOptions
			snapshot := []byte(`{"hooks":{"SessionStart":"not-an-array"}}`)
			if install {
				opts = &agenthook.InstallOptions{ConfigPath: path, Executable: "kata", Arguments: []string{"agent-contract-hook"}, Marker: "agent-contract-hook", Hooks: []agenthook.Hook{{Event: "SessionStart"}}}
				desired, err := agenthook.PlanInstall(agenthook.AgentCodex, *opts)
				require.NoError(t, err)
				var document map[string]any
				require.NoError(t, json.Unmarshal(desired.Data, &document))
				document["hooks"].(map[string]any)["Stop"] = "not-an-array"
				snapshot, err = json.Marshal(document)
				require.NoError(t, err)
			}
			_, err := planOwnedAgentHookSnapshot(agenthook.AgentCodex, path, snapshot, true, contractHook, opts)
			require.Error(t, err)
		})
	}
}

func TestAgentHooksHermesLiteralMergeKeysRemainMetadata(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	for _, key := range []string{`"<<"`, `!!str <<`} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := []byte(key + ":\n  hooks:\n    pre_llm_call:\n      - command: echo metadata\n")
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			install := func() {
				_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
				require.NoError(t, err, stderr)
			}
			install()
			require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 1)
			after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
			require.NoError(t, err)
			var beforeDoc, afterDoc map[string]any
			require.NoError(t, yaml.Unmarshal(raw, &beforeDoc))
			require.NoError(t, yaml.Unmarshal(after, &afterDoc))
			require.Equal(t, beforeDoc["<<"], afterDoc["<<"])
			install()
			again, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
			require.NoError(t, err)
			require.Equal(t, after, again)
		})
	}
}

func TestCloneAgentHookYAMLReturnsNilForMissingNode(t *testing.T) {
	require.Nil(t, cloneAgentHookYAML(nil))
	require.Nil(t, cloneAgentHookYAML(&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{nil}}))
}

func TestAgentHooksHermesPreservesYAMLNodes(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("# configuration\nlarge: 184467440737095516160\nprompt: &prompt 'kata agent-hooks contract hermes --source kata-agent-contract-hook' # custom anchor\nalias: *prompt\nhooks:\n  pre_llm_call:\n    - command: *prompt\n      args: [custom]\n    - command: 'notify kata agent-hooks contract hermes --source kata-agent-contract-hook' # wrapper\n")
	require.NoError(t, os.WriteFile(path, raw, 0o640)) //nolint:gosec // G306: deliberately verify preservation of non-default fixture permissions.
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	var node yaml.Node
	require.NoError(t, yaml.Unmarshal(after, &node))
	require.Contains(t, string(after), "184467440737095516160")
	require.Contains(t, string(after), "&prompt")
	require.Contains(t, string(after), "alias: *prompt")
	require.Contains(t, string(after), "command: *prompt")
	require.Contains(t, string(after), "# wrapper")
	require.Contains(t, string(after), "# custom anchor")
	require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 3)
	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	again, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, after, again)
}

func TestAgentHooksWrongProtocolAndCommandNameAreForeign(t *testing.T) {
	for _, tc := range []struct {
		agent   agenthook.Agent
		command string
	}{
		{agenthook.AgentClaude, "kata agent-contract-hook"},
		{agenthook.AgentHermes, "kata agent-contract-hook"},
		{agenthook.AgentCodex, "printf agent-contract-hook"},
		{agenthook.AgentCodex, "env agent-contract-hook"},
	} {
		t.Run(string(tc.agent)+tc.command, func(t *testing.T) {
			handler := map[string]any{"command": tc.command}
			hooks := map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{handler}}}}
			if tc.agent == agenthook.AgentHermes {
				hooks = map[string]any{"pre_llm_call": []any{handler}}
			}
			data, err := json.Marshal(map[string]any{"hooks": hooks})
			require.NoError(t, err)
			entries, err := parseAgentHookEntries(tc.agent, data)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, entries[0].Contract)
		})
	}
}

func TestAgentHooksCodexDefaultSupplyRequiresUnconditionalBothPlatforms(t *testing.T) {
	for _, tc := range []struct {
		name           string
		handler, group map[string]any
		want           bool
	}{
		{"default", map[string]any{"command": "kata agent-contract-hook", "commandWindows": "kata agent-contract-hook"}, nil, true},
		{"old default", map[string]any{"command": "kata agent-hooks contract codex --source kata-agent-contract-hook", "commandWindows": "kata agent-hooks contract codex --source kata-agent-contract-hook"}, nil, true},
		{"custom prompt", map[string]any{"command": "kata agent-contract-hook --source prompt.txt", "commandWindows": "kata agent-contract-hook --source prompt.txt"}, nil, false},
		{"extra args", map[string]any{"command": "kata agent-contract-hook", "commandWindows": "kata agent-contract-hook", "args": []any{"custom"}}, nil, false},
		{"condition", map[string]any{"command": "kata agent-contract-hook", "commandWindows": "kata agent-contract-hook", "if": "some condition"}, nil, false},
		{"group condition", map[string]any{"command": "kata agent-contract-hook", "commandWindows": "kata agent-contract-hook"}, map[string]any{"if": "some condition"}, false},
		{"missing Windows", map[string]any{"command": "kata agent-contract-hook"}, nil, false},
		{"wrong harness", map[string]any{"command": "kata agent-hooks contract claude", "commandWindows": "kata agent-hooks contract claude"}, nil, false},
		{"custom alternate", map[string]any{"command": "kata agent-contract-hook", "commandWindows": "kata agent-contract-hook --source windows.txt"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := map[string]any{"hooks": []any{tc.handler}}
			maps.Copy(group, tc.group)
			require.Equal(t, tc.want, codexConfigHasContract(map[string]any{"hooks": map[string]any{"SessionStart": []any{group}}}, true))
		})
	}
}

func TestAgentHooksInspectionGeneratedQuoting(t *testing.T) {
	for _, path := range []string{"/opt/example path's/bin/kata", "/opt/example\\path/kata", "/opt/example\"path/kata", `C:\Program Files\example's\kata.exe`} {
		commands, err := agenthook.BuildCommand(path, "agent-hooks", "contract", "codex")
		require.NoError(t, err)
		for _, tc := range []struct{ field, command string }{{"command", commands.Native}, {"commandWindows", commands.Windows}, {"bash", commands.POSIX}, {"powershell", commands.PowerShell}} {
			t.Run(path+tc.field, func(t *testing.T) {
				data, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"hooks": []any{map[string]any{tc.field: tc.command}}}}}})
				require.NoError(t, err)
				entries, err := parseAgentHookEntries(agenthook.AgentCodex, data)
				require.NoError(t, err)
				require.Len(t, entries, 1)
				require.True(t, entries[0].Contract)
			})
		}
	}
}

func TestAgentHooksHermesMaterializesReferencesToRemovedOwnedAnchor(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "kata agent-hooks contract hermes --source kata-agent-contract-hook"
	raw := []byte("hooks:\n  pre_llm_call:\n    - &default\n      command: '" + original + "' # original command\n      timeout: 10\n    - <<: *default\n      args: [custom]\n      integer: 184467440737095516160\n  on_session_end:\n    - <<: *default\n      args: [other]\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	var document yaml.Node
	require.NoError(t, yaml.Unmarshal(after, &document))
	var decoded map[string]any
	require.NoError(t, document.Decode(&decoded))
	hooks := decoded["hooks"].(map[string]any)
	require.Equal(t, original, hooks["pre_llm_call"].([]any)[0].(map[string]any)["command"])
	require.Equal(t, []any{"custom"}, hooks["pre_llm_call"].([]any)[0].(map[string]any)["args"])
	require.Equal(t, original, hooks["on_session_end"].([]any)[0].(map[string]any)["command"])
	require.Contains(t, string(after), "184467440737095516160")
	require.Contains(t, string(after), "# original command")
}

func TestAgentHooksHermesOwnEventAliasKeepsTemplate(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "kata agent-hooks contract hermes --source kata-agent-contract-hook"
	raw := []byte("templates: &templates\n  - command: '" + original + "'\nhooks:\n  pre_llm_call: *templates\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, original, doc["templates"].([]any)[0].(map[string]any)["command"])
	require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 1)
}

func TestProtectYAMLAgentHookCommandsRejectsMissingEntryNodes(t *testing.T) {
	entry := agentHookEntry{Event: "pre_llm_call", HandlerIndex: 0}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "missing hooks mapping", data: []byte("templates: {}\n")},
		{name: "missing event mapping", data: []byte("hooks: {}\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := protectYAMLAgentHookCommands(tc.data, []agentHookEntry{entry}, nil, contractHook, "kata-owned-hook-placeholder", map[string]string{})
			require.Error(t, err)
		})
	}
}

func TestAgentHooksInstallKeepsCanonicalContractInMixedGroup(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "hooks.json")
	commands, err := agenthook.BuildCommand(os.Args[0], "agent-hooks", "contract", "codex")
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{
		"matcher": "startup|resume|clear|compact", "note": "keep registration metadata",
		"hooks": []any{map[string]any{"command": "echo foreign"}, map[string]any{"type": "command", "command": commands.Native, "commandWindows": commands.Windows, "timeout": 10}},
	}}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	out, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "--contract-only", "codex", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.Contains(t, out, "unchanged")
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	require.Equal(t, raw, after)
}

func TestAgentHooksHermesMergedEventsPreserveForeignCommands(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("templates: &templates\n  pre_llm_call:\n    - command: kata agent-hooks contract hermes\n    - command: echo foreign\nhooks:\n  <<: *templates\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	handlers := doc["hooks"].(map[string]any)["pre_llm_call"].([]any)
	require.Len(t, handlers, 2)
	require.Equal(t, "echo foreign", handlers[0].(map[string]any)["command"])
	templates := doc["templates"].(map[string]any)["pre_llm_call"].([]any)
	require.Equal(t, "kata agent-hooks contract hermes", templates[0].(map[string]any)["command"])
}

func TestAgentHooksHermesReferencesToMutatedEventAnchorStayOriginal(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "kata agent-hooks contract hermes"
	raw := []byte("hooks:\n  pre_llm_call: &template\n    - command: " + original + "\nmirror: *template\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, []any{map[string]any{"command": original}}, doc["mirror"])
}

func TestAgentHooksDecodedScalarCannotCollideWithProtectionToken(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "hooks.json")
	raw := []byte(`{"note":"\u006bata-protected-hook-0","hooks":{"SessionStart":[{"hooks":[{"command":"echo foreign"}]}]}}`)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "codex", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	require.Equal(t, "kata-protected-hook-0", nativeHookDocument(t, agenthook.AgentCodex, path)["note"])
}

func TestAgentHooksHermesOwnedHandlerMergeKeepsTemplateCommand(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := "kata agent-hooks contract hermes"
	raw := []byte("template: &template\n  command: " + original + "\nhooks:\n  pre_llm_call:\n    - <<: *template\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, original, doc["template"].(map[string]any)["command"])
}

func TestAgentHooksHermesUninstallShadowsInheritedOwnedEvent(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("templates: &templates\n  pre_llm_call:\n    - command: kata agent-hooks contract hermes\nhooks:\n  <<: *templates\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "uninstall", "hermes", "--config", path)
	require.NoError(t, err, stderr)
	require.Empty(t, nativeContractHandlers(t, agenthook.AgentHermes, path))
	status, err := readAgentHookStatus(agenthook.AgentHermes, path, "")
	require.NoError(t, err)
	require.False(t, status.Present)
}

func TestAgentHooksHermesForeignOnlyEventAnchorMirrorStaysOriginal(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("hooks:\n  pre_llm_call: &template\n    - command: echo foreign\nmirror: *template\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, []any{map[string]any{"command": "echo foreign"}}, doc["mirror"])
	require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 2)
}

func TestAgentHooksHermesHookMappingAnchorMirrorStaysOriginal(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("hooks: &hook_map\n  pre_llm_call:\n    - command: kata agent-hooks contract hermes\nmirror: *hook_map\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, map[string]any{"pre_llm_call": []any{map[string]any{"command": "kata agent-hooks contract hermes"}}}, doc["mirror"])
}

func TestAgentHooksHermesDirectHooksAliasKeepsAnchorReferences(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("template: &hook_map\n  pre_llm_call:\n    - command: echo foreign\nhooks: *hook_map\nmirror: *hook_map\n")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	_, stderr, err := executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "install", "hermes", "--contract-only", "--config", path, "--executable", os.Args[0])
	require.NoError(t, err, stderr)
	doc := nativeHookDocument(t, agenthook.AgentHermes, path)
	expectedTemplate := map[string]any{"pre_llm_call": []any{map[string]any{"command": "echo foreign"}}}
	require.Equal(t, expectedTemplate, doc["template"])
	require.Equal(t, expectedTemplate, doc["mirror"])
	after, err := os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	require.Contains(t, string(after), "template: &hook_map")
	require.Contains(t, string(after), "mirror: *hook_map")
	require.Len(t, nativeContractHandlers(t, agenthook.AgentHermes, path), 2)
	entries, err := inspectAgentHookEntries(agenthook.AgentHermes, path)
	require.NoError(t, err)
	contractCount := 0
	for _, entry := range entries {
		if entry.Contract {
			contractCount++
		}
	}
	require.Equal(t, 1, contractCount)

	_, stderr, err = executeAgentHook(t, unreadableHookInput{}, "agent-hooks", "uninstall", "hermes", "--config", path)
	require.NoError(t, err, stderr)
	doc = nativeHookDocument(t, agenthook.AgentHermes, path)
	require.Equal(t, expectedTemplate, doc["template"])
	require.Equal(t, expectedTemplate, doc["mirror"])
	require.Equal(t, expectedTemplate, doc["hooks"])
	after, err = os.ReadFile(path) //nolint:gosec // G304: isolated installer fixture under TempDir.
	require.NoError(t, err)
	require.Contains(t, string(after), "template: &hook_map")
	require.Contains(t, string(after), "mirror: *hook_map")
}

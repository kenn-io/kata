package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/agenthook"
)

// `kata init --with-codex-hooks` wires the work.attention lifecycle into a
// Codex CLI workspace. Kit owns config parsing, hook ownership, and updates.

const (
	codexSessionStartMatcher         = "startup|resume|clear"
	codexContractSessionStartMatcher = "startup|resume|clear|compact"
	codexDefaultContractTimeoutSecs  = 10
)

func applyCodexHooks(dir string) (bool, []string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".codex", ".codex/hooks.json"); err != nil {
		return false, nil, err
	}

	configPath := filepath.Join(root.Name(), ".codex", "hooks.json")
	userPath, err := codexUserContractPath(configPath)
	if err != nil {
		return false, nil, err
	}
	warnings := codexConfigHooksWarnings(root)
	if userPath != "" {
		return applyCodexUserContract(dir, configPath, userPath, warnings)
	}
	return installCodexWorkspaceHooks(configPath, warnings)
}

func applyCodexUserContract(dir, configPath, userPath string, warnings []string) (bool, []string, error) {
	tracked, err := codexHookFileTracked(dir)
	if err != nil {
		return false, nil, err
	}
	if tracked {
		// Shared config must also serve teammates without this user's hook.
		warnings = append(warnings, fmt.Sprintf(
			"kept workspace contract hook: .codex/hooks.json is tracked; %s also injects it", userPath,
		))
		return installCodexWorkspaceHooks(configPath, warnings)
	}
	before, err := readCodexHookConfig(configPath)
	if err != nil {
		return false, nil, err
	}
	attentionChanged, err := installCodexAttentionHook(configPath, before)
	if err != nil {
		return false, nil, err
	}
	removed, notes, err := dedupeCodexContractHook(configPath, userPath, before)
	return attentionChanged || removed, append(warnings, notes...), err
}

func installCodexWorkspaceHooks(configPath string, warnings []string) (bool, []string, error) {
	before, err := readCodexHookConfig(configPath)
	if err != nil {
		return false, nil, err
	}
	attentionChanged, err := installCodexAttentionHook(configPath, before)
	if err != nil {
		return false, nil, err
	}
	contractResult, err := installOwnedAgentHooks(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"agent-contract-hook"},
		Marker:     "agent-contract-hook",
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexContractSessionStartMatcher,
			Timeout: codexDefaultContractTimeoutSecs * time.Second,
		}},
	})
	if err != nil {
		return false, nil, err
	}
	return attentionChanged || contractResult.Changed, warnings, nil
}

func installCodexAttentionHook(configPath string, before map[string]any) (bool, error) {
	attentionOptions := agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", "start"},
		Marker:     "attention-hook start",
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		}},
	}
	var attentionResult agenthook.Result
	var err error
	if !codexAttentionHookCurrent(before) {
		attentionResult, err = installOwnedAgentHooks(agenthook.AgentCodex, attentionOptions)
		if err != nil {
			return false, err
		}
	}
	endResult, err := installOwnedAgentHooks(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: configPath, Executable: "kata", Arguments: []string{"attention-hook", "end"}, Marker: "attention-hook end",
		Hooks: []agenthook.Hook{{Event: agenthook.EventSessionEnd, Timeout: 10 * time.Second}},
	})
	return attentionResult.Changed || endResult.Changed, err
}

func dedupeCodexContractHook(configPath, userPath string, before map[string]any) (bool, []string, error) {
	var result agenthook.Result
	if codexConfigHasContract(before, false) {
		var err error
		result, err = uninstallOwnedAgentHooks(agenthook.AgentCodex, configPath, contractHook)
		if err != nil {
			return false, nil, err
		}
	}
	var warnings []string
	if result.Changed {
		warnings = append(warnings, fmt.Sprintf("removed workspace contract hook: %s already injects it", userPath))
	}
	after, err := readCodexHookConfig(configPath)
	if err != nil {
		return false, nil, err
	}
	shifted, err := codexHookIndexesShifted(before, after)
	if err != nil {
		return false, nil, err
	}
	if shifted {
		warnings = append(warnings, "Codex will ask to re-trust shifted hooks; open Codex and run /hooks.")
	}
	return result.Changed, warnings, nil
}

// codexConfigHooksWarnings warns when Codex also has TOML-managed hooks.
func codexConfigHooksWarnings(root *os.Root) []string {
	const rel = ".codex/config.toml"
	if !codexConfigHasTomlHooks(root) {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s already defines a [hooks] table; kata installed its SessionStart hook into %s, which Codex loads in addition to config.toml hooks",
		filepath.Join(root.Name(), rel),
		filepath.Join(root.Name(), ".codex/hooks.json"),
	)}
}

func codexConfigHasTomlHooks(root *os.Root) bool {
	content, err := root.ReadFile(".codex/config.toml")
	if err != nil {
		return false
	}
	var parsed map[string]any
	if _, err := toml.Decode(string(content), &parsed); err != nil {
		return false
	}
	_, ok := parsed["hooks"]
	return ok
}

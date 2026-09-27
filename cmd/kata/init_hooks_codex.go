package main

import (
	"encoding/json/jsontext"
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
	userContract := false
	userPath, err := agenthook.ConfigPath(agenthook.AgentCodex)
	// An explicit workspace config does not require a resolvable user home.
	// Skip only user-path resolution failures; readable config errors still fail.
	if err == nil {
		sameConfig, err := sameCodexHookConfig(userPath, configPath)
		if err != nil {
			return false, nil, err
		}
		if !sameConfig {
			userContract, err = codexContractHookPresent(userPath)
			if err != nil {
				return false, nil, err
			}
		}
	}
	if userContract {
		tracked, err := codexHookFileTracked(dir)
		if err != nil {
			return false, nil, err
		}
		if tracked {
			// Shared config may serve teammates without this user's hook. Return
			// before migration or kit planning can serialize or mutate it.
			config, err := readCodexHookConfig(configPath)
			if err != nil {
				return false, nil, err
			}
			var warnings []string
			if codexConfigHasTomlHooks(root) {
				warnings = append(warnings, fmt.Sprintf(
					"%s already defines a [hooks] table; Codex loads it together with %s",
					filepath.Join(root.Name(), ".codex/config.toml"), configPath,
				))
			}
			if codexConfigHasContract(config) {
				warnings = append(warnings, fmt.Sprintf(
					"kept workspace contract hook: .codex/hooks.json is tracked; %s also injects it", userPath,
				))
			}
			return false, warnings, nil
		}
	}
	var before map[string]any
	if userContract {
		before, err = readCodexHookConfig(configPath)
		if err != nil {
			return false, nil, err
		}
	}
	legacyHandlers := []map[string]any{
		{
			"type":    "command",
			"command": "kata attention-hook start",
			"timeout": jsontext.Value("10"),
		},
		{
			"type":           "command",
			"command":        "kata attention-hook start",
			"commandWindows": "kata attention-hook start",
			"timeout":        jsontext.Value("10"),
		},
	}
	migrated, err := migrateLegacyAgentHooks(configPath, []legacyAgentHook{
		{
			event:         agenthook.EventSessionStart,
			matcherAbsent: true,
			handlers:      legacyHandlers,
		},
		{
			event:    agenthook.EventSessionStart,
			matcher:  codexSessionStartMatcher,
			handlers: legacyHandlers,
		},
	})
	if err != nil {
		return false, nil, err
	}
	attentionOptions := agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"agent-hooks", "attention", "start", "--source", attentionHookSource + "start"},
		Marker:     "--source " + attentionHookSource + "start",
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		}},
	}
	var attentionResult agenthook.Result
	if !userContract || !codexAttentionHookCurrent(before) {
		attentionResult, err = agenthook.Install(agenthook.AgentCodex, attentionOptions)
		if err != nil {
			return false, nil, err
		}
	}
	if userContract {
		config, err := readCodexHookConfig(configPath)
		if err != nil {
			return false, nil, err
		}
		warnings := codexConfigHooksWarnings(root)
		removed := false
		if codexConfigHasContract(config) {
			result, err := agenthook.Uninstall(agenthook.AgentCodex, configPath, "--source "+agentContractHookSource)
			if err != nil {
				return false, nil, err
			}
			removed = result.Changed
			if removed {
				warnings = append(warnings, fmt.Sprintf("removed workspace contract hook: %s already injects it", userPath))
			}
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
		return migrated || attentionResult.Changed || removed, warnings, nil
	}
	contractResult, err := agenthook.Install(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"agent-hooks", "contract", "codex", "--source", agentContractHookSource},
		Marker:     "--source " + agentContractHookSource,
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexContractSessionStartMatcher,
			Timeout: 10 * time.Second,
		}},
	})
	if err != nil {
		return false, nil, err
	}
	return migrated || attentionResult.Changed || contractResult.Changed, codexConfigHooksWarnings(root), nil
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

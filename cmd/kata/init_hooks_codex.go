package main

import (
	"encoding/json"
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
	legacyHandlers := []map[string]any{
		{
			"type":    "command",
			"command": "kata attention-hook start",
			"timeout": json.Number("10"),
		},
		{
			"type":           "command",
			"command":        "kata attention-hook start",
			"commandWindows": "kata attention-hook start",
			"timeout":        json.Number("10"),
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
	attentionResult, err := agenthook.Install(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", "start", "--source", attentionHookSource + "start"},
		Marker:     "--source " + attentionHookSource + "start",
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		}},
	})
	if err != nil {
		return false, nil, err
	}
	contractResult, err := agenthook.Install(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"agent-contract-hook", "--source", agentContractHookSource},
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
	content, err := root.ReadFile(rel)
	if err != nil {
		return nil
	}
	var parsed map[string]any
	if _, err := toml.Decode(string(content), &parsed); err != nil {
		return nil
	}
	if _, ok := parsed["hooks"]; !ok {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s already defines a [hooks] table; kata installed its SessionStart hook into %s, which Codex loads in addition to config.toml hooks",
		filepath.Join(root.Name(), rel),
		filepath.Join(root.Name(), ".codex/hooks.json"),
	)}
}

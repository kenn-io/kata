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

const codexSessionStartMatcher = "startup|resume|clear"

func applyCodexHooks(dir string) (bool, []string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".codex", ".codex/hooks.json"); err != nil {
		return false, nil, err
	}

	result, err := agenthook.Install(agenthook.AgentCodex, agenthook.InstallOptions{
		ConfigPath: filepath.Join(root.Name(), ".codex", "hooks.json"),
		Executable: "kata",
		Arguments:  []string{"attention-hook", "start"},
		Marker:     "kata attention-hook start",
		Hooks: []agenthook.Hook{{
			Event:   agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		}},
	})
	if err != nil {
		return false, nil, err
	}
	return result.Changed, codexConfigHooksWarnings(root), nil
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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"go.kenn.io/kit/agenthook"
)

// `kata init --with-codex-hooks` wires the work.attention harness into a Codex
// CLI workspace by writing .codex/hooks.json. It installs a SessionStart
// command hook for startup, resume, and clear transitions that runs
// `kata attention-hook start`. Context compaction is excluded so it cannot
// reset live attention state.
//
// Codex executes command hooks through a login shell. The bare `kata` command
// is deliberate: an absolute install path would make workspace configuration
// machine-specific, while a hostile PATH is already able to impersonate every
// developer command and is outside this installer's threat model.

const codexSessionStartMatcher = "startup|resume|clear"

// applyCodexHooks keeps kata's workspace symlink boundary and warning policy,
// then delegates normal hook ownership and config mutation to kit.
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
	if err := validateManagedHookJSON(configPath, agenthook.EventSessionStart); err != nil {
		return false, nil, err
	}
	warnings := codexConfigHooksWarnings(root)
	changed, err := installAttentionHook(
		agenthook.AgentCodex,
		configPath,
		"start",
		agenthook.Hook{
			Event:   agenthook.EventSessionStart,
			Matcher: codexSessionStartMatcher,
			Timeout: 10 * time.Second,
		},
	)
	return changed, warnings, err
}

// codexConfigHooksWarnings returns a best-effort warning when
// .codex/config.toml already defines a [hooks] table. Codex loads hooks from
// both config.toml and hooks.json, so an operator with pre-existing TOML hooks
// should know kata's hooks.json runs alongside them.
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

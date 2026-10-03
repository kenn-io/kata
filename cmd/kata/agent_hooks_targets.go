package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/agenthook"
)

type agentHookCapability struct {
	Name           string `json:"name"`
	Contract       bool   `json:"contract"`
	AttentionStart bool   `json:"attention_start"`
	AttentionEnd   bool   `json:"attention_end"`
	Project        bool   `json:"project_scope"`
	Note           string `json:"note,omitempty"`
}

func agentHookCapabilities() []agentHookCapability {
	return []agentHookCapability{
		{Name: "claude", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "codex", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true, Note: "Native SessionEnd requires a current Codex runtime; trust new hooks through /hooks."},
		{Name: "gemini", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "copilot", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "cursor", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "qwen", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "hermes", Contract: true, AttentionStart: true, AttentionEnd: true, Note: "Terminal cleanup uses on_session_finalize, not the per-turn on_session_end; approve shell hooks in Hermes."},
		{Name: "droid", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true},
		{Name: "antigravity", Contract: true, Project: true, Note: "First-invocation contract only; use launcher attention start/end."},
		{Name: "amp", Contract: true, AttentionStart: true, Project: true, Note: "Use user scope or project scopes; simultaneous owned user/project installs are rejected because Amp plugins run in separate processes. No native session-end event; use launcher attention end. Hidden context requires the May 26 2026 plugin SDK or later."},
		{Name: "opencode", Contract: true, AttentionStart: true, Project: true, Note: "Setup detects stable OpenCode v1 (>=1.0.154) or v2 (>=2.0.0); no native session-close event, so use launcher attention end."},
		{Name: "pi", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true, Note: "Pi >=0.99.1; reload and complete native project trust. Offline status cannot confirm runtime loading."},
		{Name: "openclaw", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true, Note: "OpenClaw >=2026.9.7; prompt and attention need authorized workspace capture. Inspect/reload the native plugin before use."},
		{Name: "kimi-code", Contract: true, AttentionStart: true, AttentionEnd: true, Note: "Contract uses UserPromptSubmit plain output; SessionStart does not consume context. Project discovery is unverified."},
		{Name: "kimi", AttentionStart: true, AttentionEnd: true, Note: "Archived Kimi CLI supports attention only; its runtime discards allowing hook context. Use committed AGENTS.md for the contract."},
		{Name: "muse", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true, Note: "Ordinary hooks strip Kata environment. Native attention requires authorized user managed forwarding, or launcher attention."},
		{Name: "grok", AttentionStart: true, AttentionEnd: true, Project: true, Note: "Official xAI Grok supports attention only. It also scans Claude/Cursor hook sources; avoid overlapping attention installations."},
		{Name: "zcode", Contract: true, AttentionStart: true, Note: "Official Z.ai ZCode has no native SessionEnd; use launcher attention end. Project discovery is unverified."},
	}
}
func lookupAgentHookCapability(name string) (agentHookCapability, error) {
	canonical, err := canonicalNativeAttentionTarget(name)
	if err != nil {
		var names []string
		for _, capability := range agentHookCapabilities() {
			names = append(names, capability.Name)
		}
		return agentHookCapability{}, agentHookUsage(fmt.Sprintf("unknown harness %q; choose %s", name, strings.Join(names, ", ")))
	}
	for _, capability := range agentHookCapabilities() {
		if capability.Name == canonical {
			return capability, nil
		}
	}
	return agentHookCapability{}, agentHookUsage(fmt.Sprintf("unknown harness %q", name))
}
func agentHookUsesKit(name string) bool {
	switch name {
	case "claude", "codex", "gemini", "copilot", "cursor", "qwen", "hermes":
		return true
	}
	return false
}

func agentHookScopePath(target, scope, dir string) (string, error) {
	capability, err := lookupAgentHookCapability(target)
	if err != nil {
		return "", err
	}
	target = capability.Name
	if scope != "user" && scope != "project" {
		return "", agentHookUsage("--scope must be user or project")
	}
	if scope == "project" {
		if !capability.Project {
			return "", agentHookUsage(target + " has no verified project hook discovery; use --scope user")
		}
		paths := map[string]string{
			"claude": ".claude/settings.json", "codex": ".codex/hooks.json", "gemini": ".gemini/settings.json", "copilot": ".github/hooks/kata.json", "cursor": ".cursor/hooks.json", "qwen": ".qwen/settings.json", "droid": ".factory/hooks.json", "antigravity": ".agents/hooks.json", "pi": ".pi/extensions/kata.js", "amp": ".amp/plugins/kata-project.js", "openclaw": ".openclaw/extensions/kata", "muse": ".muse/hooks.json", "grok": ".grok/hooks/kata.json",
		}
		if target == "opencode" {
			return filepath.Abs(openCodeAgentHookFallbackPath(filepath.Join(dir, ".opencode"), scope))
		}
		return filepath.Abs(filepath.Join(dir, filepath.FromSlash(paths[target])))
	}
	if agentHookUsesKit(target) {
		return agenthook.ConfigPath(agenthook.Agent(target))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	rootPath := func(env, defaultDir, file string) string {
		root := os.Getenv(env)
		if root == "" {
			root = defaultDir
		}
		return filepath.Join(root, file)
	}
	switch target {
	case "droid":
		return filepath.Join(home, ".factory", "hooks.json"), nil
	case "antigravity":
		return filepath.Join(home, ".gemini", "config", "hooks.json"), nil
	case "pi":
		root := os.Getenv("PI_CODING_AGENT_DIR")
		if root == "" {
			root = filepath.Join(home, ".pi", "agent")
		} else if root == "~" {
			root = home
		} else if suffix, ok := strings.CutPrefix(root, "~/"); ok {
			root = filepath.Join(home, suffix)
		}
		if !filepath.IsAbs(root) {
			return "", agentHookUsage("PI_CODING_AGENT_DIR must resolve to an absolute path")
		}
		return filepath.Join(root, "extensions", "kata.js"), nil
	case "amp":
		return filepath.Join(configHome, "amp", "plugins", "kata-user.js"), nil
	case "opencode":
		return openCodeAgentHookFallbackPath(filepath.Join(configHome, "opencode"), scope), nil
	case "openclaw":
		_, state, err := openClawAgentHookPaths(home, "")
		if err != nil {
			return "", err
		}
		return filepath.Join(state, "extensions", "kata-hooks-user"), nil
	case "kimi-code":
		return rootPath("KIMI_CODE_HOME", filepath.Join(home, ".kimi-code"), "config.toml"), nil
	case "kimi":
		return filepath.Join(home, ".kimi", "config.toml"), nil
	case "muse":
		return filepath.Join(configHome, "muse", "settings.json"), nil
	case "grok":
		return rootPath("GROK_HOME", filepath.Join(home, ".grok"), "hooks/kata.json"), nil
	case "zcode":
		return filepath.Join(home, ".zcode", "cli", "config.json"), nil
	}
	return "", agentHookUsage("unknown harness")
}

// Reporting defaults to the published v1 discovery path. A real v2 entrypoint
// is sufficient to report its package path without probing an agent version.
func openCodeAgentHookFallbackPath(root, scope string) string {
	v2 := filepath.Join(root, "plugins", "kata-"+scope, "index.js")
	if info, err := os.Stat(v2); err == nil && !info.IsDir() { //nolint:gosec // G703: metadata-only reporting of an explicitly selected native config root; installation separately validates every path.
		return v2
	}
	return filepath.Join(root, "plugin", "kata-"+scope+".js")
}

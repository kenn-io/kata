package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func resolveNativeAgentHookScope(cmd *cobra.Command, scope string, local bool) (string, error) {
	if local {
		if cmd.Flags().Changed("scope") && scope != "project" {
			return "", agentHookUsage("--local conflicts with --scope " + scope)
		}
		scope = "project"
	}
	if scope != "user" && scope != "project" {
		return "", agentHookUsage("--scope must be user or project")
	}
	return scope, nil
}

func resolveNativeAgentHookAttention(cmd *cobra.Command, attention, contractOnly, managed bool) (bool, error) {
	if contractOnly && cmd.Flags().Changed("attention") && attention {
		return false, agentHookUsage("--contract-only conflicts with --attention")
	}
	if managed && (contractOnly || !attention) {
		return false, agentHookUsage("--managed-attention conflicts with contract-only selection")
	}
	return attention && !contractOnly, nil
}

// Dedicated agent roots are configuration signals. Shared workspace directories
// such as .github and .agents require a native configuration file instead.
func nativeAgentHookConfiguredTarget(target nativeAgentHookTarget) (bool, error) {
	configured, err := nativeAgentHookConfigurationExists(target.capability.Name, target.options.Scope, target.path)
	if err != nil || configured || target.options.Scope != "project" {
		return configured, err
	}
	userPath, err := agentHookScopePath(target.capability.Name, "user", target.options.Dir)
	if err != nil {
		return false, err
	}
	return nativeAgentHookConfigurationExists(target.capability.Name, "user", userPath)
}

func nativeAgentHookConfigurationExists(name, scope, path string) (bool, error) {
	root := nativeAgentHookConfigRoot(name, scope, path)
	if scope == "project" && (name == "copilot" || name == "antigravity") {
		candidates := []string{path}
		if name == "copilot" {
			candidates = append(candidates, filepath.Join(root, "copilot-instructions.md"))
			files, err := filepath.Glob(filepath.Join(root, "hooks", "*.json"))
			if err != nil {
				return false, err
			}
			candidates = append(candidates, files...)
		}
		for _, candidate := range candidates {
			info, err := os.Stat(candidate)
			if err == nil && info.Mode().IsRegular() {
				return true, nil
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
		return false, nil
	}
	if name == "opencode" && strings.HasSuffix(path, string(filepath.Separator)+"index.js") {
		root = filepath.Dir(root)
	}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

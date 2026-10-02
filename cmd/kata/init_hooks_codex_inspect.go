package main

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kit/agenthook"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// sameCodexHookConfig also recognizes user config links to the workspace file.
// One file is one installation, even if Codex reaches it through two paths.
func sameCodexHookConfig(userPath, workspacePath string) (bool, error) {
	userAbs, err := filepath.Abs(userPath)
	if err != nil {
		return false, err
	}
	workspaceAbs, err := filepath.Abs(workspacePath)
	if err != nil {
		return false, err
	}
	if userAbs == workspaceAbs {
		return true, nil
	}
	userInfo, err := os.Stat(userPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	workspaceInfo, err := os.Stat(workspacePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(userInfo, workspaceInfo), nil
}

// codexUserContractPath returns the separate user config supplying the contract.
func codexUserContractPath(workspacePath string) (string, error) {
	userPath, err := agenthook.ConfigPath(agenthook.AgentCodex)
	// An explicit workspace config does not require a resolvable user home.
	if err != nil {
		return "", nil
	}
	sameConfig, err := sameCodexHookConfig(userPath, workspacePath)
	if err != nil || sameConfig {
		return "", err
	}
	present, err := codexContractHookPresent(userPath)
	if err != nil || !present {
		return "", err
	}
	return userPath, nil
}

func codexContractHookPresent(path string) (bool, error) {
	// Kit validates the native hook layout and ownership without editing it.
	// Changed alone is insufficient: it can reflect another event or empty groups.
	if _, err := agenthook.PlanUninstall(agenthook.AgentCodex, path, "--source "+legacyAgentContractHookSource); err != nil {
		return false, err
	}
	parsed, err := readCodexHookConfig(path)
	if err != nil {
		return false, err
	}
	return codexConfigHasContract(parsed, true), nil
}

// readCodexHookConfig is inspection only. Kit owns all config mutations.
func readCodexHookConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // selected user config or rooted, symlink-checked workspace path
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var parsed map[string]any
	// Preserve numeric literals for full-field equality and survivor identities,
	// including values beyond float64 precision. Unmarshal also rejects trailing JSON.
	if err := json.Unmarshal(data, &parsed, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, err
	}
	return parsed, nil
}

func codexConfigHasContract(parsed map[string]any, requireFullMatcher bool) bool {
	hooks, _ := parsed["hooks"].(map[string]any)
	groups, _ := hooks[string(agenthook.EventSessionStart)].([]any)
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		if requireFullMatcher && !codexMatcherCoversContract(group) {
			continue
		}
		handlers, _ := group["hooks"].([]any)
		for _, rawHandler := range handlers {
			handler, _ := rawHandler.(map[string]any)
			entry := makeAgentHookEntry(agenthook.AgentCodex, string(agenthook.EventSessionStart), 0, 0, group, handler)
			if entry.Contract && (!requireFullMatcher || effectiveAgentHookDefault(agenthook.AgentCodex, entry)) {
				return true
			}
		}
	}
	return false
}

func codexMatcherCoversContract(group map[string]any) bool {
	raw, exists := group["matcher"]
	if !exists {
		return true
	}
	matcher, ok := raw.(string)
	if !ok {
		return false
	}
	if matcher == "" || matcher == "*" {
		return true
	}
	pattern, err := regexp.Compile(matcher)
	if err != nil {
		return false
	}
	for source := range strings.SplitSeq(codexContractSessionStartMatcher, "|") {
		if !pattern.MatchString(source) {
			return false
		}
	}
	return true
}

func codexHookFileTracked(dir string) (bool, error) {
	discovered, err := config.DiscoverPaths(dir)
	if err != nil {
		return false, err
	}
	if discovered.GitRoot == "" {
		return false, nil
	}
	// Run from the workspace so path matching remains correct in nested repos
	// and linked worktrees; git/cmd strips inherited GIT_* routing variables.
	stdout, _, err := gitcmd.New().Run(context.Background(), dir, nil, "ls-files", "-z", "--", ".codex/hooks.json")
	if err != nil {
		return false, fmt.Errorf("check tracked Codex hooks: %w", err)
	}
	for path := range bytes.SplitSeq(stdout, []byte{0}) {
		if string(path) == ".codex/hooks.json" {
			return true, nil
		}
	}
	return false, nil
}

// An unchanged handler keeps its Codex trust key even in a mixed group. Kit
// Install would extract it and append a new group, moving it and other hooks.
func codexAttentionHookCurrent(parsed map[string]any) bool {
	expected := map[string]any{
		"type":           "command",
		"command":        "kata attention-hook start",
		"commandWindows": "kata attention-hook start",
		"timeout":        jsontext.Value("10"),
	}
	hooks, _ := parsed["hooks"].(map[string]any)
	owned := 0
	current := false
	for event, rawGroups := range hooks {
		groups, _ := rawGroups.([]any)
		for _, rawGroup := range groups {
			group, _ := rawGroup.(map[string]any)
			handlers, _ := group["hooks"].([]any)
			for _, rawHandler := range handlers {
				handler, _ := rawHandler.(map[string]any)
				if classifyAgentHookHandler(agenthook.AgentCodex, handler) != attentionStartHook {
					continue
				}
				owned++
				current = event == string(agenthook.EventSessionStart) &&
					group["matcher"] == codexSessionStartMatcher && reflect.DeepEqual(handler, expected)
			}
		}
	}
	return owned == 1 && current
}

type codexHookPosition struct {
	group   int
	handler int
}

// Compare the coordinates of unchanged survivors, not the number of removed
// hooks. Pair identical registrations in occurrence order within each event.
func codexHookIndexesShifted(before, after map[string]any) (bool, error) {
	original, err := codexHookPositions(before)
	if err != nil {
		return false, err
	}
	remaining, err := codexHookPositions(after)
	if err != nil {
		return false, err
	}
	for identity, positions := range original {
		for index, position := range positions {
			survivors := remaining[identity]
			if index < len(survivors) && position != survivors[index] {
				return true, nil
			}
		}
	}
	return false, nil
}

func codexHookPositions(parsed map[string]any) (map[string][]codexHookPosition, error) {
	positions := make(map[string][]codexHookPosition)
	hooks, _ := parsed["hooks"].(map[string]any)
	for event, rawGroups := range hooks {
		groups, _ := rawGroups.([]any)
		for groupIndex, rawGroup := range groups {
			group, _ := rawGroup.(map[string]any)
			metadata := make(map[string]any, len(group))
			for key, value := range group {
				if key != "hooks" {
					metadata[key] = value
				}
			}
			handlers, _ := group["hooks"].([]any)
			for handlerIndex, handler := range handlers {
				identity, err := json.Marshal([]any{event, metadata, handler}, json.Deterministic(true))
				if err != nil {
					return nil, err
				}
				key := string(identity)
				positions[key] = append(positions[key], codexHookPosition{group: groupIndex, handler: handlerIndex})
			}
		}
	}
	return positions, nil
}

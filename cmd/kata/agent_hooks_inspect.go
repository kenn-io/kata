package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"go.kenn.io/kit/agenthook"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/jsonutil"
	"gopkg.in/yaml.v3"
)

const agentContractMarker = "--source " + legacyAgentContractHookSource

type agentHookEntry struct {
	Event            string
	GroupIndex       int
	HandlerIndex     int
	Matcher          string
	Command          string
	AlternateCommand string
	Fields           map[string]any
	GroupFields      map[string]any
	Kind             agentHookKind
	Contract         bool
	Attention        bool
}

// Kit validates native structure before these shared ownership and no-op reads.
func inspectAgentHookEntries(agent agenthook.Agent, path string) ([]agentHookEntry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the selected agent config.
	if errors.Is(err, os.ErrNotExist) {
		return []agentHookEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := validateAgentHookSnapshot(agent, path, data); err != nil {
		return nil, err
	}
	return parseAgentHookEntries(agent, data)
}

func parseAgentHookEntries(agent agenthook.Agent, data []byte) ([]agentHookEntry, error) {
	var document map[string]any
	if len(bytes.TrimSpace(data)) == 0 {
		return []agentHookEntry{}, nil
	}
	if agent == agenthook.AgentHermes {
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, err
		}
	} else {
		if err := json.Unmarshal(data, &document, jsonutil.PreserveNumberLiterals()); err != nil {
			return nil, err
		}
	}
	hooks, _ := document["hooks"].(map[string]any)
	events := make([]string, 0, len(hooks))
	for event := range hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	entries := []agentHookEntry{}
	for _, event := range events {
		groups, _ := hooks[event].([]any)
		for index, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if agent != agenthook.AgentCursor && agent != agenthook.AgentCopilot && agent != agenthook.AgentHermes {
				handlers, _ := group["hooks"].([]any)
				for handlerIndex, rawHandler := range handlers {
					handler, ok := rawHandler.(map[string]any)
					if ok {
						entries = append(entries, makeAgentHookEntry(agent, event, index, handlerIndex, group, handler))
					}
				}
			} else {
				entries = append(entries, makeAgentHookEntry(agent, event, -1, index, nil, group))
			}
		}
	}
	return entries, nil
}

func makeAgentHookEntry(agent agenthook.Agent, event string, groupIndex, handlerIndex int, group, handler map[string]any) agentHookEntry {
	matcher := handler["matcher"]
	if group != nil {
		matcher = group["matcher"]
	}

	entry := agentHookEntry{Event: event, GroupIndex: groupIndex, HandlerIndex: handlerIndex, Fields: map[string]any{}, GroupFields: map[string]any{}}
	for key, value := range group {
		if key != "hooks" {
			entry.GroupFields[key] = value
		}
	}
	// Every handler field participates: fields such as args and if change
	// execution semantics even when the command and timeout are unchanged.
	maps.Copy(entry.Fields, handler)
	if matcher != nil {
		entry.Fields["matcher"] = matcher
	}
	entry.Matcher, _ = matcher.(string)
	entry.Command, _ = handler["command"].(string)
	if entry.Command == "" {
		entry.Command, _ = handler["bash"].(string)
	}
	entry.AlternateCommand, _ = handler["commandWindows"].(string)
	if entry.AlternateCommand == "" {
		entry.AlternateCommand, _ = handler["powershell"].(string)
	}
	entry.Kind = classifyAgentHookHandler(agent, handler)
	entry.Contract = entry.Kind == contractHook
	entry.Attention = entry.Kind == attentionStartHook || entry.Kind == attentionEndHook
	return entry
}

func ownedContractMatches(current, planned []agentHookEntry) bool {
	return ownedAgentHookEntriesMatch(current, planned, contractHook)
}

func agentHookContractEvent(agent agenthook.Agent) string {
	switch agent {
	case agenthook.AgentHermes:
		return "pre_llm_call"
	case agenthook.AgentCursor:
		return "sessionStart"
	default:
		return string(agenthook.EventSessionStart)
	}
}

func hasAgentHookContract(agent agenthook.Agent, entries []agentHookEntry) bool {
	for _, entry := range entries {
		if effectiveAgentHookDefault(agent, entry) && entry.Event == agentHookContractEvent(agent) && kitAgentHookMatcherCoversLifecycle(agent, entry) {
			return true
		}
	}
	return false
}

func agentHookWorkspacePath() (string, error) {
	if flags.Workspace != "" {
		return filepath.Abs(flags.Workspace)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	discovered, err := config.DiscoverPaths(cwd)
	if err != nil {
		return "", err
	}
	return config.WriteDestination(discovered, cwd), nil
}

func sameAgentHookFile(a, b string) bool {
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil && aAbs == bAbs {
		return true
	}
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

func agentHookDuplicateHint(agent agenthook.Agent, userPath, workspace string) (string, error) {
	if agent != agenthook.AgentCodex {
		return "", nil
	}
	workspacePath := filepath.Join(workspace, ".codex", "hooks.json")
	if sameAgentHookFile(userPath, workspacePath) {
		return "", nil
	}
	entries, err := inspectAgentHookEntries(agent, workspacePath)
	if err != nil {
		return "", fmt.Errorf("inspect workspace hooks: %w", err)
	}
	if hasAgentHookContract(agent, entries) {
		tracked, err := codexHookFileTracked(workspace)
		if err != nil {
			return "", err
		}
		if tracked {
			return "workspace duplicate is in tracked .codex/hooks.json; kata init --with-codex-hooks keeps shared hooks for teammates", nil
		}
		return "run kata init --with-codex-hooks here to drop the workspace duplicate", nil
	}
	return "", nil
}

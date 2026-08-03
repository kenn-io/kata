package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"go.kenn.io/kit/agenthook"
)

type legacyAgentHook struct {
	event         agenthook.Event
	matcher       string
	matcherAbsent bool
	handlers      []map[string]any
}

// migrateLegacyAgentHooks removes only complete handler shapes kata installed
// before source-marked ownership. This one-way upgrade migration is scheduled
// for removal by kata task m7d5 on or after 2026-11-02.
func migrateLegacyAgentHooks(configPath string, legacy []legacyAgentHook) (bool, error) {
	data, err := os.ReadFile(configPath) //nolint:gosec // workspace path is rooted and symlink-checked by the caller
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return false, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return false, err
	}

	changed := false
	for _, candidate := range legacy {
		removed, err := removeExactLegacyAgentHook(root, candidate)
		if err != nil {
			return false, err
		}
		changed = changed || removed
	}
	if !changed {
		return false, nil
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil { //nolint:gosec // existing workspace config; mode is preserved
		return false, err
	}
	return true, nil
}

func removeExactLegacyAgentHook(root map[string]any, legacy legacyAgentHook) (bool, error) {
	rawHooks, exists := root["hooks"]
	if !exists {
		return false, nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return false, errors.New("hooks has an unexpected shape")
	}
	rawGroups, exists := hooks[string(legacy.event)]
	if !exists {
		return false, nil
	}
	groups, ok := rawGroups.([]any)
	if !ok {
		return false, fmt.Errorf("hooks.%s has an unexpected shape", legacy.event)
	}

	changed := false
	keptGroups := make([]any, 0, len(groups))
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok || !legacyMatcherMatches(group, legacy) {
			keptGroups = append(keptGroups, rawGroup)
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			return false, fmt.Errorf("hooks.%s group hooks has an unexpected shape", legacy.event)
		}
		keptHandlers := make([]any, 0, len(handlers))
		groupChanged := false
		for _, handler := range handlers {
			if exactLegacyHandler(handler, legacy.handlers) {
				changed = true
				groupChanged = true
				continue
			}
			keptHandlers = append(keptHandlers, handler)
		}
		if !groupChanged {
			keptGroups = append(keptGroups, rawGroup)
			continue
		}
		if len(keptHandlers) == 0 && len(group) == 2 {
			continue
		}
		group["hooks"] = keptHandlers
		keptGroups = append(keptGroups, group)
	}
	if !changed {
		return false, nil
	}
	if len(keptGroups) == 0 {
		delete(hooks, string(legacy.event))
	} else {
		hooks[string(legacy.event)] = keptGroups
	}
	return true, nil
}

func legacyMatcherMatches(group map[string]any, legacy legacyAgentHook) bool {
	rawMatcher, exists := group["matcher"]
	if legacy.matcherAbsent {
		return !exists
	}
	matcher, ok := rawMatcher.(string)
	return exists && ok && matcher == legacy.matcher
}

func exactLegacyHandler(handler any, candidates []map[string]any) bool {
	for _, candidate := range candidates {
		if reflect.DeepEqual(handler, candidate) {
			return true
		}
	}
	return false
}

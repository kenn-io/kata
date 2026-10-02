package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"go.kenn.io/kata/internal/jsonutil"
)

type extraJSONRegistration struct {
	block, event  string
	group, index  int
	handler       map[string]any
	unconditional bool
}

func parseExtraJSON(data []byte, exists bool) (map[string]any, error) {
	if !exists {
		return map[string]any{}, nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("native hook configuration is empty")
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("native hook configuration must be an object")
	}
	return root, nil
}

func extraJSONObject(parent map[string]any, key string) (map[string]any, error) {
	if value, exists := parent[key]; exists {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", key)
		}
		return object, nil
	}
	object := map[string]any{}
	parent[key] = object
	return object, nil
}

func planExtraJSONHooks(opts nativeAgentHookOptions, remove bool, path string, data []byte, exists, wrappedDroid bool) (nativeAgentHookPlan, []byte, error) {
	root, err := parseExtraJSON(data, exists)
	if err != nil {
		return nativeAgentHookPlan{}, nil, err
	}
	sections := map[string]map[string]any{}
	nonemptyBlocks := map[string]bool{}
	globalEnabled := true
	globalDisabledPolicy := false
	if opts.Agent == "antigravity" {
		for name, value := range root {
			object, ok := value.(map[string]any)
			if !ok {
				return nativeAgentHookPlan{}, nil, fmt.Errorf("Antigravity hook %q must be an object", name) //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
			}
			if enabled, exists := object["enabled"]; exists {
				if _, ok := enabled.(bool); !ok {
					return nativeAgentHookPlan{}, nil, errors.New("Antigravity enabled must be boolean") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
				}
			}
			sections[name] = object
			nonemptyBlocks[name] = len(object) > 0
		}
	} else {
		section := root
		if opts.Agent != "droid" || wrappedDroid {
			section, err = extraJSONObject(root, "hooks")
			if err != nil {
				return nativeAgentHookPlan{}, nil, err
			}
		}
		if opts.Agent == "zcode" {
			globalEnabled = section["enabled"] == true
			globalDisabledPolicy = section["enabled"] == false
			if enabled, exists := section["enabled"]; exists {
				if _, ok := enabled.(bool); !ok {
					return nativeAgentHookPlan{}, nil, errors.New("ZCode hooks.enabled must be boolean")
				}
			}
			section, err = extraJSONObject(section, "events")
			if err != nil {
				return nativeAgentHookPlan{}, nil, err
			}
		}
		sections[""] = section
	}
	registrations, err := extraJSONRegistrations(opts.Agent, sections)
	if err != nil {
		return nativeAgentHookPlan{}, nil, err
	}
	museTypesValid := opts.Agent != "muse" || extraMuseRegistrationTypesValid(registrations)
	var current [3]bool
	partialRegistration := false
	for _, registration := range registrations {
		kind := extraHookKind(opts.Agent, registration.handler)
		if index := extraKindIndex(kind); index >= 0 && extraJSONRegistrationEligible(opts.Agent, registration, kind) && globalEnabled && museTypesValid {
			current[index] = true
		} else if kind != "" {
			partialRegistration = true
		}
	}
	desired := extraHookDesired(opts, remove, current)
	plan := extraHookPlan(path, current, desired, opts.Agent)
	if !museTypesValid {
		plan.Warnings = append(plan.Warnings, "Muse "+path+" has malformed handler field types; the whole native source is inactive until those fields are repaired")
	}
	if partialRegistration {
		plan.Warnings = append(plan.Warnings, "some owned registrations are disabled, conditional, or unable to return context; reinstall the selected components to repair them")
	}
	if globalDisabledPolicy {
		plan.Warnings = append(plan.Warnings, "ZCode hooks.enabled=false is an existing operator policy; enable it in the native config to activate Kata hooks")
	}
	if remove {
		selectedOwned := false
		for _, registration := range registrations {
			kind := extraHookKind(opts.Agent, registration.handler)
			if (kind == contractHook && opts.Contract) || ((kind == attentionStartHook || kind == attentionEndHook) && opts.Attention) {
				selectedOwned = true
				break
			}
		}
		if !selectedOwned {
			return plan, bytes.Clone(data), nil
		}
	}
	// Reconciliation keeps already-correct owned handlers in place. It removes
	// only exact owned commands, even when an operator added platform variants.
	for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
		index := extraKindIndex(kind)
		var owned []extraJSONRegistration
		for _, registration := range registrations {
			if extraHookKind(opts.Agent, registration.handler) == kind {
				owned = append(owned, registration)
			}
		}
		selected := opts.Contract
		if kind != contractHook {
			selected = opts.Attention
		}
		if !selected {
			continue
		}
		want := desired[index]
		var handler map[string]any
		if want {
			handler, err = extraHookHandler(opts, kind)
			if err != nil {
				return nativeAgentHookPlan{}, nil, err
			}
		}
		if want && len(owned) == 1 && extraJSONRegistrationEligible(opts.Agent, owned[0], kind) && extraJSONHandlerMatches(owned[0].handler, handler) {
			continue
		}
		for _, section := range sections {
			removeExtraJSONKind(opts.Agent, section, kind)
		}
		if want {
			section := sections[""]
			if opts.Agent == "antigravity" {
				name := "kata"
				for n := 2; ; n++ {
					if _, exists := root[name]; !exists {
						break
					}
					name = fmt.Sprintf("kata-%d", n)
				}
				section = map[string]any{}
				root[name] = section
				sections[name] = section
			}
			if section == nil {
				return nativeAgentHookPlan{}, nil, errors.New("native hook event section is unavailable")
			}
			event := extraHookEvent(opts.Agent, kind)
			items, _ := section[event].([]any)
			if opts.Agent == "antigravity" {
				section[event] = append(items, handler)
			} else {
				section[event] = append(items, map[string]any{"hooks": []any{handler}})
			}
		}
	}
	if opts.Agent == "antigravity" {
		for name, section := range sections {
			if len(section) == 0 && nonemptyBlocks[name] {
				delete(root, name)
			}
		}
	}
	if opts.Agent == "zcode" && !remove && (desired[0] || desired[1]) {
		if !globalDisabledPolicy {
			root["hooks"].(map[string]any)["enabled"] = true
			globalEnabled = true
		}
	}
	if !globalEnabled {
		plan.Contract, plan.AttentionStart, plan.AttentionEnd = false, false, false
	}
	if opts.Agent == "muse" {
		// Native validation rejects the entire source for one malformed typed
		// field, even on a foreign sibling. Recompute after selected owned
		// handlers are repaired or removed, preserving foreign handlers.
		renderedRegistrations, err := extraJSONRegistrations(opts.Agent, sections)
		if err != nil {
			return nativeAgentHookPlan{}, nil, err
		}
		var configured [3]bool
		if extraMuseRegistrationTypesValid(renderedRegistrations) {
			for _, registration := range renderedRegistrations {
				kind := extraHookKind(opts.Agent, registration.handler)
				if index := extraKindIndex(kind); index >= 0 && extraJSONRegistrationEligible(opts.Agent, registration, kind) {
					configured[index] = true
				}
			}
		}
		plan.Contract, plan.AttentionStart, plan.AttentionEnd = configured[0], configured[1], configured[2]
	}
	content, err := renderExtraJSON(data, root)
	if !exists && remove {
		content = nil
	}
	return plan, content, err
}

func extraJSONRegistrationEligible(target string, registration extraJSONRegistration, kind agentHookKind) bool {
	if registration.event != extraHookEvent(target, kind) || !registration.unconditional || registration.handler["enabled"] == false {
		return false
	}
	contract, start, end := extraHookCapabilities(target)
	if (kind == contractHook && !contract) || (kind == attentionStartHook && !start) || (kind == attentionEndHook && !end) {
		return false
	}
	if kind == contractHook && registration.handler["async"] == true {
		return false
	}
	command, _ := registration.handler["command"].(string)
	if target == "muse" && runtime.GOOS == "windows" {
		for _, field := range []string{"commandWindows", "command_windows"} {
			if alternate, ok := registration.handler[field].(string); ok && alternate != "" {
				command = alternate
				break
			}
		}
	}
	if strings.TrimSpace(command) == "" {
		return false
	}
	if target == "muse" {
		if registration.handler["type"] != "command" || !extraMuseHandlerTypesValid(registration.handler) {
			return false
		}
		for field := range registration.handler {
			switch field {
			case "type", "command", "commandWindows", "command_windows", "timeout", "statusMessage", "async", "onFailure", "silent":
			default:
				return false
			}
		}
	}
	return true
}

// Presence remains separate from eligibility when reconciling a managed bundle:
// an inactive exact command is still owned and must not acquire a duplicate.
func extraJSONHasOwnedContract(target string, root map[string]any) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	registrations, err := extraJSONRegistrations(target, map[string]map[string]any{"": hooks})
	if err != nil {
		return false
	}
	for _, registration := range registrations {
		if extraHookKind(target, registration.handler) == contractHook {
			return true
		}
	}
	return false
}

func extraJSONRegistrations(target string, sections map[string]map[string]any) ([]extraJSONRegistration, error) {
	var registrations []extraJSONRegistration
	for block, section := range sections {
		for event, value := range section {
			if target == "antigravity" && event == "enabled" {
				continue
			}
			if target == "muse" && event == "state" {
				if _, ok := value.(map[string]any); !ok {
					return nil, errors.New("Muse hooks.state must be an object") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
				}
				continue
			}
			items, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("native hook event %q must be an array", event)
			}
			flat := target == "antigravity" && (event == "PreInvocation" || event == "PostInvocation" || event == "Stop")
			for groupIndex, raw := range items {
				group, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("native hook event %q requires object handlers", event)
				}
				if flat {
					registrations = append(registrations, extraJSONRegistration{block: block, event: event, group: -1, index: groupIndex, handler: group, unconditional: section["enabled"] != false})
					continue
				}
				handlers, ok := group["hooks"].([]any)
				if !ok || len(handlers) == 0 {
					return nil, fmt.Errorf("native hook event %q requires a nonempty hooks array", event)
				}
				matcher := group["matcher"]
				if matcher != nil {
					if _, ok := matcher.(string); !ok {
						return nil, errors.New("native hook matcher must be a string")
					}
				}
				for index, rawHandler := range handlers {
					handler, ok := rawHandler.(map[string]any)
					if !ok {
						return nil, errors.New("native hook handler must be an object")
					}
					registrations = append(registrations, extraJSONRegistration{block: block, event: event, group: groupIndex, index: index, handler: handler, unconditional: matcher == nil || matcher == "" || matcher == "*"})
				}
			}
		}
	}
	return registrations, nil
}

func extraJSONHandlerMatches(current, desired map[string]any) bool {
	// JSON-decoded numeric literals and generated integer timeout values have
	// different Go types. Compare their JSON representation, preserving extras.
	for key, want := range desired {
		have, exists := current[key]
		if !exists {
			return false
		}
		left, _ := json.Marshal(have)
		right, _ := json.Marshal(want)
		if !bytes.Equal(left, right) {
			return false
		}
	}
	if current["enabled"] == false || current["async"] == true {
		return false
	}
	return true
}

func removeExtraJSONKind(target string, section map[string]any, kind agentHookKind) {
	for event, value := range section {
		items, ok := value.([]any)
		if !ok {
			continue
		}
		flat := target == "antigravity" && (event == "PreInvocation" || event == "PostInvocation" || event == "Stop")
		kept := make([]any, 0, len(items))
		for _, raw := range items {
			group := raw.(map[string]any)
			if flat {
				if extraHookKind(target, group) != kind {
					kept = append(kept, group)
				}
				continue
			}
			handlers := group["hooks"].([]any)
			remaining := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				if extraHookKind(target, rawHandler.(map[string]any)) != kind {
					remaining = append(remaining, rawHandler)
				}
			}
			if len(remaining) == 0 {
				continue
			}
			if !reflect.DeepEqual(handlers, remaining) {
				group["hooks"] = remaining
			}
			kept = append(kept, group)
		}
		if len(kept) == 0 && len(items) > 0 {
			delete(section, event)
		} else if !reflect.DeepEqual(items, kept) {
			section[event] = kept
		}
	}
}

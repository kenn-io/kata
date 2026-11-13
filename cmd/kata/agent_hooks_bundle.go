package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kit/agenthook"
	"gopkg.in/yaml.v3"
)

func planKitAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	agent := agenthook.Agent(opts.Agent)
	plan := nativeAgentHookPlan{Path: opts.ConfigPath, Warnings: []string{}}
	original, exists, err := readNativeAgentHookFile(opts.ConfigPath)
	if err != nil {
		return plan, err
	}
	current, err := parseAgentHookEntries(agent, original)
	if err != nil {
		return plan, err
	}
	plan.CurrentContract, plan.CurrentAttentionStart, plan.CurrentAttentionEnd = kitAgentHookConfigured(agent, current)
	for _, entry := range current {
		plan.CurrentOwnedContract = plan.CurrentOwnedContract || entry.Kind == contractHook
		plan.CurrentOwnedAttention = plan.CurrentOwnedAttention || entry.Kind == attentionStartHook || entry.Kind == attentionEndHook
	}
	data := bytes.Clone(original)
	dataExists := exists
	for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
		selected := opts.Contract
		if kind != contractHook {
			selected = opts.Attention
		}
		if !selected {
			continue
		}
		var install *agenthook.InstallOptions
		if !remove {
			options := kitAgentHookRegistration(opts, kind)
			install = &options
		}
		if agent == agenthook.AgentHermes && kind != contractHook {
			data, err = planHermesLifecycleSnapshot(data, kind, install)
		} else {
			var next ownedAgentHookPlan
			next, err = planOwnedAgentHookSnapshot(agent, opts.ConfigPath, data, dataExists, kind, install)
			data = next.result.Data
		}
		if err != nil {
			return plan, err
		}
		if len(data) > 0 {
			dataExists = true
		}
	}
	desired, err := parseAgentHookEntries(agent, data)
	if err != nil {
		return plan, err
	}
	plan.Contract, plan.AttentionStart, plan.AttentionEnd = kitAgentHookConfigured(agent, desired)
	// Include unchanged configs as preimages in the shared transaction.
	plan.Changes = []nativeAgentHookChange{{Path: opts.ConfigPath, Original: original, OriginalExists: exists, Content: data, Remove: !dataExists}}
	if agent == agenthook.AgentCodex && dataExists && nativeAgentHookPlanChanged(plan) {
		before := map[string]any{}
		if len(bytes.TrimSpace(original)) > 0 {
			if err := json.Unmarshal(original, &before, jsonutil.PreserveNumberLiterals()); err != nil {
				return plan, err
			}
		}
		var after map[string]any
		if err := json.Unmarshal(data, &after, jsonutil.PreserveNumberLiterals()); err != nil {
			return plan, err
		}
		shifted, err := codexHookIndexesShifted(before, after)
		if err != nil {
			return plan, err
		}
		warning := "Codex will ask to re-trust shifted hooks; open Codex and run /hooks."
		if shifted && !slices.Contains(plan.Warnings, warning) {
			plan.Warnings = append(plan.Warnings, warning)
		}
	}
	return plan, nil
}

func kitAgentHookRegistration(opts nativeAgentHookOptions, kind agentHookKind) agenthook.InstallOptions {
	agent := agenthook.Agent(opts.Agent)
	args := []string{"agent-hook", "contract", opts.Agent}
	hook := contractRegistrationHook(agent)
	if kind != contractHook {
		args = []string{"agent-hook", "attention-native", opts.Agent, string(kind)}
		hook = agenthook.Hook{Event: agenthook.EventSessionStart, Timeout: 10 * time.Second}
		if kind == attentionEndHook {
			hook.Event = agenthook.EventSessionEnd
		}
		if agent == agenthook.AgentClaude {
			for _, spec := range claudeHookSpecs() {
				if spec.event == hook.Event {
					hook.Matcher = spec.matcher
					break
				}
			}
		} else if agent == agenthook.AgentCodex && kind == attentionStartHook {
			hook.Matcher = "startup|resume|clear"
		}
	}
	marker := "contract"
	source := legacyAgentContractHookSource
	if kind != contractHook {
		marker = "attention-native " + opts.Agent + " " + string(kind)
		source = legacyAttentionHookSource + string(kind)
	}
	args = agentHookOwnershipArgs(opts.Executable, args, kind)
	if opts.RequireOwnershipMarker {
		if len(args) < 2 || !slices.Equal(args[len(args)-2:], []string{"--source", source}) {
			args = append(args, "--source", source)
		}
		marker = "--source " + source
	}
	return agenthook.InstallOptions{ConfigPath: opts.ConfigPath, Executable: opts.Executable, Arguments: args, Marker: marker, Hooks: []agenthook.Hook{hook}}
}

func kitAgentHookConfigured(agent agenthook.Agent, entries []agentHookEntry) (contract, start, end bool) {
	for _, entry := range entries {
		// Eligibility is independent of ownership: an owned command can still
		// be disabled or conditional and must not count as configured coverage.
		eligible := entry
		eligible.Contract = true
		if !effectiveAgentHookDefault(agent, eligible) || !kitAgentHookMatcherCoversLifecycle(agent, entry) {
			continue
		}
		switch entry.Kind {
		case contractHook:
			contract = contract || strings.EqualFold(entry.Event, agentHookContractEvent(agent))
		case attentionStartHook:
			expected := "SessionStart"
			if agent == agenthook.AgentHermes {
				expected = "on_session_start"
			}
			start = start || strings.EqualFold(entry.Event, expected)
		case attentionEndHook:
			expected := "SessionEnd"
			if agent == agenthook.AgentHermes {
				expected = "on_session_finalize"
			}
			end = end || strings.EqualFold(entry.Event, expected)
		}
	}
	return
}

func kitAgentHookMatcherCoversLifecycle(agent agenthook.Agent, entry agentHookEntry) bool {
	if entry.Matcher == "" || entry.Matcher == "*" {
		return true
	}
	// Gemini and Qwen use exact lifecycle sources, so one restrictive matcher
	// cannot cover every start source. Other non-regex codecs also need an
	// unconditional registration for complete coverage.
	if agent != agenthook.AgentClaude && agent != agenthook.AgentCodex {
		return false
	}
	if entry.Kind == attentionEndHook {
		if agent == agenthook.AgentClaude {
			for _, spec := range claudeHookSpecs() {
				if spec.event == agenthook.EventSessionEnd {
					return entry.Matcher == spec.matcher
				}
			}
		}
		return entry.Matcher == ".*" || entry.Matcher == "^.*$"
	}
	pattern, err := regexp.Compile(entry.Matcher)
	if err != nil {
		return false
	}
	sources := []string{"startup", "resume", "clear"}
	if entry.Kind == contractHook {
		sources = append(sources, "compact")
	}
	for _, source := range sources {
		if !pattern.MatchString(source) {
			return false
		}
	}
	return true
}

// Hermes's terminal hook is distinct from Kit's per-turn on_session_end. Reuse
// the exact command/alias ownership shield, then mutate only owned lifecycle
// handlers through YAML nodes so foreign events, styles and mirrors survive.
func planHermesLifecycleSnapshot(data []byte, kind agentHookKind, opts *agenthook.InstallOptions) ([]byte, error) {
	current, err := parseAgentHookEntries(agenthook.AgentHermes, data)
	if err != nil {
		return nil, err
	}
	event := "on_session_start"
	if kind == attentionEndHook {
		event = "on_session_finalize"
	}
	var desired []agentHookEntry
	if opts != nil {
		command, err := agenthook.BuildCommand(opts.Executable, opts.Arguments...)
		if err != nil {
			return nil, err
		}
		desired = []agentHookEntry{makeAgentHookEntry(agenthook.AgentHermes, event, -1, 0, nil, map[string]any{"command": command.POSIX, "timeout": 10})}
		if kind == attentionStartHook {
			desired = append(desired, makeAgentHookEntry(agenthook.AgentHermes, "on_session_reset", -1, 0, nil, map[string]any{"command": command.POSIX, "timeout": 10}))
		}
		if ownedAgentHookEntriesMatch(current, desired, kind) {
			return data, nil
		}
	} else {
		found := false
		for _, entry := range current {
			found = found || entry.Kind == kind
		}
		if !found {
			return data, nil
		}
	}
	marker := "kata-owned-lifecycle-placeholder"
	protected, restore, err := protectAgentHookCommands(agenthook.AgentHermes, data, current, desired, kind, marker)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err = yaml.Unmarshal(protected, &document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("hermes config must be a mapping")
	}
	root := document.Content[0]
	hooks := yamlAgentHookField(root, "hooks")
	if hooks == nil {
		hooks = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hooks"}, hooks)
	}
	if hooks.Kind != yaml.MappingNode {
		return nil, errors.New("hermes hooks must be a mapping")
	}
	for i := 1; i < len(hooks.Content); i += 2 {
		sequence := yamlAgentHookResolved(hooks.Content[i])
		if sequence == nil || sequence.Kind != yaml.SequenceNode {
			continue
		}
		kept := make([]*yaml.Node, 0, len(sequence.Content))
		for _, handler := range sequence.Content {
			command := yamlAgentHookField(handler, "command")
			if command != nil && command.Value == marker {
				continue
			}
			kept = append(kept, handler)
		}
		sequence.Content = kept
	}
	if opts != nil {
		for _, entry := range desired {
			sequence := yamlAgentHookDirectField(hooks, entry.Event)
			if sequence == nil {
				sequence = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				hooks.Content = append(hooks.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry.Event}, sequence)
			}
			if sequence.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("hermes %s hooks must be an array", entry.Event)
			}
			handler := &yaml.Node{}
			if err = handler.Encode(map[string]any{"command": entry.Command, "timeout": 10}); err != nil {
				return nil, err
			}
			sequence.Content = append(sequence.Content, handler)
		}
	}

	encoded, err := yaml.Marshal(&document)
	if err != nil {
		return nil, err
	}
	return restore(encoded)
}

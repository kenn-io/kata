package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pathresolve"
	"gopkg.in/yaml.v3"
)

// Released source markers identify built-in hooks, including renamed executables.
const (
	legacyAgentContractHookSource = "kata-agent-contract-hook"
	legacyAttentionHookSource     = "kata-agent-hook-"
)

type agentHookKind string

const (
	contractHook       agentHookKind = "contract"
	attentionStartHook agentHookKind = "start"
	attentionEndHook   agentHookKind = "end"
)

var agentHookCommandFields = []string{"command", "commandWindows", "bash", "powershell"}

// Ownership is an exact direct invocation, across every populated platform.
// An explicit built-in source marker also identifies a renamed executable.
func classifyAgentHookHandler(agent agenthook.Agent, handler map[string]any) agentHookKind {
	if raw, exists := handler["type"]; exists && raw != "command" {
		return ""
	}
	var kind agentHookKind
	legacySplit := false
	if args, exists := handler["args"]; exists {
		values, ok := args.([]any)
		if !ok {
			return ""
		}
		if len(values) > 0 {
			if agent != agenthook.AgentClaude || len(values) != 2 || values[0] != "attention-hook" || (values[1] != "start" && values[1] != "end") {
				return ""
			}
			command, _ := handler["command"].(string)
			if command != "kata" {
				return ""
			}
			kind = agentHookKind(values[1].(string))
			legacySplit = true
		}
	}
	populated := false
	for _, field := range agentHookCommandFields {
		raw, exists := handler[field]
		if !exists {
			continue
		}
		command, ok := raw.(string)
		if !ok {
			return ""
		}
		if command == "" {
			continue
		}
		populated = true
		candidate := classifyAgentHookCommand(agent, command, field)
		if legacySplit && field == "command" {
			candidate = kind
		}
		if candidate == "" || (kind != "" && candidate != kind) {
			return ""
		}
		kind = candidate
	}
	if !populated {
		return ""
	}
	return kind
}

func classifyAgentHookCommand(agent agenthook.Agent, command, field string) agentHookKind {
	powershell := field == "powershell"
	command = strings.TrimSpace(command)
	if powershell && strings.HasPrefix(command, "& ") {
		command = strings.TrimSpace(command[1:])
	}
	// Parse only literal argv. No substitutions, shell operators or assignments.
	argv, ok := literalAgentHookWords(command, powershell, field == "commandWindows" || (field == "command" && runtime.GOOS == "windows"))
	if !ok || len(argv) < 2 {
		return ""
	}
	executable := argv[0]
	if executable == "" || (strings.Contains(executable, "=") && !strings.ContainsAny(executable, "/\\")) {
		return ""
	}
	if !isKataHookExecutable(executable) && !strings.ContainsAny(executable, "/\\") {
		return ""
	}
	args := argv[1:]
	var kind agentHookKind
	var tail []string
	switch {
	case args[0] == "agent-contract-hook" && agent == agenthook.AgentCodex:
		kind = contractHook
		tail = args[1:]
	case args[0] == "attention-hook" && len(args) >= 2 && (args[1] == "start" || args[1] == "end"):
		kind = agentHookKind(args[1])
		tail = args[2:]
	case len(args) >= 3 && args[0] == "agent-hooks" && args[1] == "contract" && args[2] == string(agent):
		kind = contractHook
		tail = args[3:]
	case len(args) >= 4 && args[0] == "agent-hooks" && args[1] == "attention-native" && args[2] == string(agent) && (args[3] == "start" || args[3] == "end"):
		kind = agentHookKind(args[3])
		tail = args[4:]
	case len(args) >= 3 && args[0] == "agent-hooks" && args[1] == "attention" && (args[2] == "start" || args[2] == "end"):
		kind = agentHookKind(args[2])
		tail = args[3:]
	default:
		return ""
	}
	if len(tail) == 0 && isKataHookExecutable(executable) {
		return kind
	}
	source := legacyAgentContractHookSource
	if kind != contractHook {
		source = legacyAttentionHookSource + string(kind)
	}
	if slices.Equal(tail, []string{"--source", source}) {
		return kind
	}
	return ""
}

func isKataHookExecutable(executable string) bool {
	// Inspect both separators regardless of the platform reading the config.
	name := executable[strings.LastIndexAny(executable, "/\\")+1:]
	return name == "kata" || name == "kata.exe"
}

func agentHookOwnershipArgs(executable string, args []string, kind agentHookKind) []string {
	if isKataHookExecutable(executable) {
		return args
	}
	source := legacyAgentContractHookSource
	if kind != contractHook {
		source = legacyAttentionHookSource + string(kind)
	}
	if len(args) >= 2 && slices.Equal(args[len(args)-2:], []string{"--source", source}) {
		return args
	}
	return append(args, "--source", source)
}

func literalAgentHookWords(command string, powershell, windows bool) ([]string, bool) {
	var words []string
	var word strings.Builder
	quoted := byte(0)
	started := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quoted == '\'' {
			if c == '\'' {
				if powershell && i+1 < len(command) && command[i+1] == '\'' {
					word.WriteByte(c)
					i++
				} else {
					quoted = 0
				}
			} else {
				word.WriteByte(c)
			}
			continue
		}
		if quoted == '"' {
			if c == '"' {
				quoted = 0
				continue
			}
			if !windows && strings.ContainsRune("$`", rune(c)) {
				return nil, false
			}
			if c == '\\' && i+1 < len(command) && (windows || strings.ContainsRune("\"\\$`", rune(command[i+1]))) {
				if windows { // CommandLineToArgvW escapes consecutive backslashes before quotes.
					j := i
					for j < len(command) && command[j] == '\\' {
						j++
					}
					if j < len(command) && command[j] == '"' {
						word.WriteString(strings.Repeat("\\", (j-i)/2))
						if (j-i)%2 == 0 {
							quoted = 0
						} else {
							word.WriteByte('"')
						}
						i = j
						continue
					}
					word.WriteString(command[i:j])
					i = j - 1
					continue
				}
				i++
				c = command[i]
			}
			word.WriteByte(c)
			continue
		}
		switch c {
		case ' ', '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		case '\n', '\r', ';', '|', '&', '<', '>', '(', ')', '$', '`', '*', '?', '[', ']', '{', '}', '#':
			return nil, false
		case '\'', '"':
			if windows && c == '\'' {
				word.WriteByte(c)
			} else {
				quoted = c
			}
			started = true
		case '\\':
			if windows || powershell {
				word.WriteByte(c)
			} else {
				i++
				if i >= len(command) {
					return nil, false
				}
				word.WriteByte(command[i])
			}
			started = true
		default:
			word.WriteByte(c)
			started = true
		}
	}
	if quoted != 0 {
		return nil, false
	}
	if started {
		words = append(words, word.String())
	}
	return words, len(words) > 0
}

// A repairable registration may still be conditional or platform incomplete.
// Such a registration must not suppress the workspace's built-in default.
func effectiveAgentHookDefault(agent agenthook.Agent, entry agentHookEntry) bool {
	if !entry.Contract {
		return false
	}
	for key, value := range entry.Fields {
		switch key {
		case "type", "command", "commandWindows", "bash", "powershell", "timeout", "timeoutSec", "matcher":
		case "args":
			args, ok := value.([]any)
			if !ok || len(args) != 0 {
				return false
			}
		default:
			return false
		}
	}
	for key := range entry.GroupFields {
		if key != "matcher" && key != "hooks" {
			return false
		}
	}
	if agent == agenthook.AgentCodex {
		if timeout, exists := entry.Fields["timeout"]; exists && !sufficientCodexContractTimeout(timeout) {
			return false
		}
		windows, _ := entry.Fields["commandWindows"].(string)
		return entry.Command != "" && windows != ""
	}
	if agent == agenthook.AgentCopilot {
		bash, _ := entry.Fields["bash"].(string)
		powershell, _ := entry.Fields["powershell"].(string)
		return bash != "" && powershell != ""
	}
	return entry.Command != ""
}

func sufficientCodexContractTimeout(value any) bool {
	raw, ok := value.(jsontext.Value)
	if !ok {
		return false
	}
	seconds, err := strconv.ParseInt(string(raw), 10, 64)
	return err == nil && seconds >= codexDefaultContractTimeoutSecs
}

func ownedAgentHookEntriesMatch(current, desired []agentHookEntry, kind agentHookKind) bool {
	owned := func(entries []agentHookEntry) []agentHookEntry {
		var result []agentHookEntry
		for _, entry := range entries {
			if classifyAgentHookHandlerFromEntry(entry) == kind {
				result = append(result, entry)
			}
		}
		return result
	}
	before, after := owned(current), owned(desired)
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i].Event != after[i].Event || !reflect.DeepEqual(before[i].Fields, after[i].Fields) || !sameAgentHookGroupBehavior(before[i].GroupFields, after[i].GroupFields) {
			return false
		}
	}
	return true
}

// Non-execution group metadata stays in place when a built-in handler already
// matches. Conditional behavior still requires repair to the requested default.
func sameAgentHookGroupBehavior(before, after map[string]any) bool {
	for _, key := range []string{"if", "condition", "enabled", "disabled", "async", "once"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			return false
		}
	}
	return true
}

func classifyAgentHookHandlerFromEntry(entry agentHookEntry) agentHookKind {
	if entry.Contract {
		return contractHook
	}
	if entry.Attention {
		return entry.Kind
	}
	return ""
}

// Kit builds and validates native registrations. Its substring removal is
// bounded by temporarily masking every foreign handler's command scalars.
// The staging config is never published; only the restored result is written.
type ownedAgentHookPlan struct {
	result         agenthook.Result
	original       []byte
	originalExists bool
}

func planOwnedAgentHookInstall(agent agenthook.Agent, opts agenthook.InstallOptions) (ownedAgentHookPlan, error) {
	commands, err := agenthook.BuildCommand("kata", opts.Arguments...)
	if err != nil {
		return ownedAgentHookPlan{}, err
	}
	kind := classifyAgentHookCommand(agent, commands.POSIX, "bash")
	if kind == "" {
		return ownedAgentHookPlan{}, errors.New("installer requires a built-in hook command")
	}
	opts.Arguments = agentHookOwnershipArgs(opts.Executable, opts.Arguments, kind)
	return planOwnedAgentHookMutation(agent, opts.ConfigPath, kind, &opts)
}
func installOwnedAgentHooks(agent agenthook.Agent, opts agenthook.InstallOptions) (agenthook.Result, error) {
	plan, err := planOwnedAgentHookInstall(agent, opts)
	if err != nil {
		return plan.result, err
	}
	return writeOwnedAgentHookPlan(plan)
}
func planOwnedAgentHookUninstall(agent agenthook.Agent, path string, kind agentHookKind) (ownedAgentHookPlan, error) {
	return planOwnedAgentHookMutation(agent, path, kind, nil)
}
func uninstallOwnedAgentHooks(agent agenthook.Agent, path string, kind agentHookKind) (agenthook.Result, error) {
	plan, err := planOwnedAgentHookUninstall(agent, path, kind)
	if err != nil {
		return plan.result, err
	}
	return writeOwnedAgentHookPlan(plan)
}

func planOwnedAgentHookMutation(agent agenthook.Agent, path string, kind agentHookKind, opts *agenthook.InstallOptions) (ownedAgentHookPlan, error) {
	original, err := os.ReadFile(path) //nolint:gosec // path comes from the selected agent hook target.
	originalExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ownedAgentHookPlan{}, err
	}
	if err := validateAgentHookSnapshot(agent, path, original); err != nil {
		return ownedAgentHookPlan{}, err
	}
	return planOwnedAgentHookSnapshot(agent, path, original, originalExists, kind, opts)
}

// Derive handler indexes and staged bytes from the same captured config, even
// when another installer replaces the original path while this plan is built.
func planOwnedAgentHookSnapshot(agent agenthook.Agent, path string, original []byte, originalExists bool, kind agentHookKind, opts *agenthook.InstallOptions) (ownedAgentHookPlan, error) {
	captured := bytes.Clone(original)
	plan := ownedAgentHookPlan{
		result:         agenthook.Result{Agent: agent, ConfigPath: path, Data: bytes.Clone(captured)},
		original:       captured,
		originalExists: originalExists,
	}
	stage, err := os.MkdirTemp("", "kata-agent-hooks-")
	if err != nil {
		return plan, err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	stagePath := filepath.Join(stage, "config")
	validationData := original
	if agent == agenthook.AgentHermes {
		validationData, err = cloneYAMLAgentHooksAlias(original)
		if err != nil {
			return plan, err
		}
	}
	if err := os.WriteFile(stagePath, validationData, 0o600); err != nil { //nolint:gosec // G703: OS-created temporary directory plus fixed basename.
		return plan, err
	}
	if _, err := agenthook.PlanUninstall(agent, stagePath, agentContractMarker); err != nil {
		return plan, err
	}
	current, err := parseAgentHookEntries(agent, original)
	if err != nil {
		return plan, err
	}
	marker := "kata-owned-hook-placeholder"
	var desiredEntries []agentHookEntry
	if opts != nil {
		options := *opts
		options.ConfigPath = filepath.Join(stage, "desired-config")
		desired, err := agenthook.PlanInstall(agent, options)
		if err != nil {
			return plan, err
		}
		expected, err := parseAgentHookEntries(agent, desired.Data)
		if err != nil {
			return plan, err
		}
		desiredEntries = expected
		if ownedAgentHookEntriesMatch(current, expected, kind) {
			return plan, nil
		}
		marker = options.Marker
	} else {
		hasOwned := false
		for _, entry := range current {
			hasOwned = hasOwned || classifyAgentHookHandlerFromEntry(entry) == kind
		}
		if !hasOwned {
			return plan, nil
		}
	}
	staged, restore, err := protectAgentHookCommands(agent, original, current, desiredEntries, kind, marker)
	if err != nil {
		return plan, err
	}
	if err := os.WriteFile(stagePath, staged, 0o600); err != nil { //nolint:gosec // G703: OS-created temporary directory plus fixed basename; no caller path enters staging.
		return plan, err
	}
	var planned agenthook.Result
	if opts != nil {
		options := *opts
		options.ConfigPath = stagePath
		planned, err = agenthook.PlanInstall(agent, options)
	} else {
		planned, err = agenthook.PlanUninstall(agent, stagePath, marker)
	}
	if err != nil {
		return plan, err
	}
	restored, err := restore(planned.Data)
	if err != nil {
		return plan, err
	}
	plan.result.Data = restored
	plan.result.Changed = true
	return plan, nil
}

func protectAgentHookCommands(agent agenthook.Agent, data []byte, entries, desired []agentHookEntry, kind agentHookKind, marker string) ([]byte, func([]byte) ([]byte, error), error) {
	// Collision checks operate on decoded scalars: JSON/YAML escapes can conceal
	// a token in the original bytes that would otherwise corrupt unrelated data.
	token, err := agentHookProtectionToken(agent, data)
	if err != nil {
		return nil, nil, err
	}
	replacements := map[string]string{}
	for _, entry := range entries {
		for _, field := range agentHookCommandFields {
			value, _ := entry.Fields[field].(string)
			if _, exists := replacements[value]; value != "" && !exists {
				replacements[value] = fmt.Sprintf("%s%d", token, len(replacements))
			}
		}
	}
	if agent == agenthook.AgentHermes {
		return protectYAMLAgentHookCommands(data, entries, desired, kind, marker, replacements)
	}
	document := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &document, jsonutil.PreserveNumberLiterals()); err != nil {
			return nil, nil, err
		}
	}
	hooks, _ := document["hooks"].(map[string]any)
	for _, entry := range entries {
		groups := hooks[entry.Event].([]any)
		var handler map[string]any
		if entry.GroupIndex >= 0 {
			handler = groups[entry.GroupIndex].(map[string]any)["hooks"].([]any)[entry.HandlerIndex].(map[string]any)
		} else {
			handler = groups[entry.HandlerIndex].(map[string]any)
		}
		for _, field := range agentHookCommandFields {
			if value, ok := handler[field].(string); ok && value != "" {
				handler[field] = replacements[value]
			}
		}
		if classifyAgentHookHandlerFromEntry(entry) == kind {
			if agent == agenthook.AgentCopilot {
				handler["bash"] = marker
			} else {
				handler["command"] = marker
			}
		}
	}
	staged, err := marshalAgentHookJSON(document)
	restore := func(planned []byte) ([]byte, error) {
		var root map[string]any
		if err := json.Unmarshal(planned, &root, jsonutil.PreserveNumberLiterals()); err != nil {
			return nil, err
		}
		restoreAgentHookJSONScalars(root, replacements)
		return marshalAgentHookJSON(root)
	}
	return staged, restore, err
}
func marshalAgentHookJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value, jsontext.WithIndent("  "), json.Deterministic(true))
	return append(data, '\n'), err
}
func restoreAgentHookJSONScalars(value any, replacements map[string]string) {
	restore := func(s string) string {
		for original, token := range replacements {
			if s == token {
				return original
			}
		}
		return s
	}
	rewriteAgentHookJSONScalars(value, restore)
}
func rewriteAgentHookJSONScalars(value any, replace func(string) string) {
	switch node := value.(type) {
	case map[string]any:
		for key, value := range node {
			if s, ok := value.(string); ok {
				node[key] = replace(s)
			} else {
				rewriteAgentHookJSONScalars(value, replace)
			}
		}
	case []any:
		for i, value := range node {
			if s, ok := value.(string); ok {
				node[i] = replace(s)
			} else {
				rewriteAgentHookJSONScalars(value, replace)
			}
		}
	}
}

func protectYAMLAgentHookCommands(data []byte, entries, desired []agentHookEntry, kind agentHookKind, marker string, replacements map[string]string) ([]byte, func([]byte) ([]byte, error), error) {
	document := yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, nil, err
		}
	}
	walkYAMLAgentHookScalars(&document, func(s string) string {
		if token, ok := replacements[s]; ok {
			return token
		}
		return s
	})
	if _, err := materializeYAMLAgentHooksAlias(&document); err != nil {
		return nil, nil, err
	}
	hooks := yamlAgentHookField(document.Content[0], "hooks")
	// Kit requires explicit native mappings. Materialize inherited hook/event
	// fields locally so installing one event cannot shadow foreign merged hooks.
	root := document.Content[0]
	if hooks != nil && yamlAgentHookDirectField(root, "hooks") == nil {
		cloned := cloneAgentHookYAML(hooks)
		if cloned == nil {
			return nil, nil, errors.New("hooks contain an unresolved YAML alias")
		}
		hooks = cloned
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "hooks"}, hooks)
	}
	for _, entry := range entries {
		if hooks == nil {
			return nil, nil, errors.New("hook entries do not match a Hermes hooks mapping")
		}
		if yamlAgentHookDirectField(hooks, entry.Event) == nil {
			inherited := yamlAgentHookField(hooks, entry.Event)
			if inherited == nil {
				return nil, nil, fmt.Errorf("hook event %q is missing from the YAML snapshot", entry.Event)
			}
			cloned := cloneAgentHookYAML(inherited)
			if cloned == nil {
				return nil, nil, fmt.Errorf("hook event %q contains an unresolved YAML alias", entry.Event)
			}
			hooks.Content = append(hooks.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: entry.Event}, cloned)
		}
	}

	mutatedEvents := map[string]bool{}
	shadows := map[string]bool{}
	for _, entry := range desired {
		mutatedEvents[entry.Event] = true
	}
	for _, entry := range entries {
		if classifyAgentHookHandlerFromEntry(entry) == kind {
			mutatedEvents[entry.Event] = true
		}
		if yamlAgentHookMergedField(hooks, entry.Event, map[*yaml.Node]bool{}) != nil {
			shadows[entry.Event] = true
		}
	}
	// Event aliases become local sequences before mutation, preserving templates.
	for eventName := range mutatedEvents {
		event := yamlAgentHookField(hooks, eventName)
		if event != nil && event.Kind == yaml.AliasNode {
			cloned := cloneAgentHookYAML(event)
			if cloned == nil {
				return nil, nil, fmt.Errorf("hook event %q contains an unresolved YAML alias", eventName)
			}
			*event = *cloned
		}
	}
	removedAnchors := map[*yaml.Node]bool{}
	var rememberAnchors func(*yaml.Node)
	rememberAnchors = func(node *yaml.Node) {
		if node.Anchor != "" {
			removedAnchors[node] = true
		}
		for _, child := range node.Content {
			rememberAnchors(child)
		}
	}
	for _, entry := range entries {
		if classifyAgentHookHandlerFromEntry(entry) == kind {
			event := yamlAgentHookResolved(yamlAgentHookField(hooks, entry.Event))
			if event == nil || entry.HandlerIndex < 0 || entry.HandlerIndex >= len(event.Content) {
				return nil, nil, fmt.Errorf("hook event %q does not contain the planned handler", entry.Event)
			}
			rememberAnchors(event.Content[entry.HandlerIndex])
			if event.Anchor != "" {
				removedAnchors[event] = true
			}
		}
	}

	// Kit also mutates the hooks map and appended-to event containers, even when
	// the event initially has only foreign handlers. Their mirrors stay original.
	if hooks != nil && hooks.Anchor != "" {
		removedAnchors[hooks] = true
	}
	for eventName := range mutatedEvents {
		event := yamlAgentHookResolved(yamlAgentHookField(hooks, eventName))
		if event != nil && event.Anchor != "" {
			removedAnchors[event] = true
		}
	}
	// Materialize only references to definitions that will disappear. The copied
	// nodes retain their literal values, comments and styles without dangling aliases.
	var preserveReferences func(*yaml.Node)
	preserveReferences = func(node *yaml.Node) {
		if node.Kind == yaml.AliasNode && removedAnchors[node.Alias] {
			if clone := cloneAgentHookYAML(node); clone != nil {
				*node = *clone
			}
		}
		for _, child := range node.Content {
			preserveReferences(child)
		}
	}
	preserveReferences(&document)

	for _, entry := range entries {
		if classifyAgentHookHandlerFromEntry(entry) != kind {
			continue
		}
		group := yamlAgentHookResolved(yamlAgentHookField(hooks, entry.Event))
		if group == nil || entry.HandlerIndex < 0 || entry.HandlerIndex >= len(group.Content) {
			return nil, nil, fmt.Errorf("hook event %q does not contain the planned handler", entry.Event)
		}
		handler := group.Content[entry.HandlerIndex]
		if handler.Kind == yaml.AliasNode {
			handler = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, handler}}
			group.Content[entry.HandlerIndex] = handler
		}
		command := yamlAgentHookDirectField(handler, "command")
		if command == nil {
			handler.Content = append(handler.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "command"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: marker})
		} else {
			*command = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: marker}
		}
	}
	staged, err := yaml.Marshal(&document)
	restore := func(planned []byte) ([]byte, error) {
		var node yaml.Node
		if err := yaml.Unmarshal(planned, &node); err != nil {
			return nil, err
		}

		// An empty local override must survive removal; otherwise YAML merging
		// re-exposes the inherited event that uninstall just removed.
		plannedHooks := yamlAgentHookField(node.Content[0], "hooks")
		for eventName := range shadows {
			if yamlAgentHookDirectField(plannedHooks, eventName) == nil {
				plannedHooks.Content = append(plannedHooks.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: eventName}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
			}
		}
		reverse := map[string]string{}
		for original, token := range replacements {
			reverse[token] = original
		}
		walkYAMLAgentHookScalars(&node, func(s string) string {
			if original, ok := reverse[s]; ok {
				return original
			}
			return s
		})
		return yaml.Marshal(&node)
	}
	return staged, restore, err
}

func cloneYAMLAgentHooksAlias(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return data, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	changed, err := materializeYAMLAgentHooksAlias(&document)
	if err != nil {
		return nil, err
	}
	if !changed {
		return data, nil
	}
	return yaml.Marshal(&document)
}

func validateAgentHookSnapshot(agent agenthook.Agent, path string, data []byte) error {
	_, nativeErr := agenthook.PlanUninstall(agent, path, agentContractMarker)
	if nativeErr == nil || agent != agenthook.AgentHermes {
		return nativeErr
	}
	validationData, err := cloneYAMLAgentHooksAlias(data)
	if err != nil || bytes.Equal(validationData, data) {
		return nativeErr
	}
	stage, err := os.MkdirTemp("", "kata-agent-hooks-validate-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	stagePath := filepath.Join(stage, "config")
	if err := os.WriteFile(stagePath, validationData, 0o600); err != nil { //nolint:gosec // G703: OS-created temporary directory plus fixed basename.
		return err
	}
	if _, err := agenthook.PlanUninstall(agent, stagePath, agentContractMarker); err != nil {
		return nativeErr
	}
	return nil
}

func materializeYAMLAgentHooksAlias(document *yaml.Node) (bool, error) {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0] == nil {
		return false, errors.New("hermes YAML document is missing its root mapping")
	}
	root := document.Content[0]
	hooks := yamlAgentHookDirectField(root, "hooks")
	if hooks == nil || hooks.Kind != yaml.AliasNode {
		return false, nil
	}
	cloned := cloneAgentHookYAML(hooks)
	if cloned == nil {
		return false, errors.New("hooks contain an unresolved YAML alias")
	}
	*hooks = *cloned
	return true, nil
}

func yamlAgentHookResolved(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}
func yamlAgentHookDirectField(node *yaml.Node, key string) *yaml.Node {
	node = yamlAgentHookResolved(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
func yamlAgentHookField(node *yaml.Node, key string) *yaml.Node {
	return yamlAgentHookFieldSeen(node, key, map[*yaml.Node]bool{})
}
func yamlAgentHookFieldSeen(node *yaml.Node, key string, seen map[*yaml.Node]bool) *yaml.Node {
	node = yamlAgentHookResolved(node)
	if node == nil || seen[node] {
		return nil
	}
	if field := yamlAgentHookDirectField(node, key); field != nil {
		return field
	}
	seen[node] = true
	defer delete(seen, node)
	return yamlAgentHookMergedField(node, key, seen)
}
func yamlAgentHookMergedField(node *yaml.Node, key string, seen map[*yaml.Node]bool) *yaml.Node {
	var merge *yaml.Node
	if node != nil && node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Tag == "!!merge" && node.Content[i].Value == "<<" {
				merge = yamlAgentHookResolved(node.Content[i+1])
				break
			}
		}
	}
	if merge != nil && merge.Kind == yaml.SequenceNode {
		for _, candidate := range merge.Content {
			if field := yamlAgentHookFieldSeen(candidate, key, seen); field != nil {
				return field
			}
		}
		return nil
	}
	if merge == nil {
		return nil
	}
	return yamlAgentHookFieldSeen(merge, key, seen)
}

func agentHookProtectionToken(agent agenthook.Agent, data []byte) (string, error) {
	token := "kata-protected-hook-"
	var document any
	var node yaml.Node
	if len(bytes.TrimSpace(data)) > 0 {
		if agent == agenthook.AgentHermes {
			if err := yaml.Unmarshal(data, &node); err != nil {
				return "", err
			}
		} else if err := json.Unmarshal(data, &document, jsonutil.PreserveNumberLiterals()); err != nil {
			return "", err
		}
	}
	for {
		collision := false
		check := func(value string) string { collision = collision || strings.Contains(value, token); return value }
		if agent == agenthook.AgentHermes {
			walkYAMLAgentHookScalars(&node, check)
		} else {
			rewriteAgentHookJSONScalars(document, check)
		}
		if !collision {
			return token, nil
		}
		token += "x"
	}
}

// cloneAgentHookYAML resolves aliases while retaining scalar encoding metadata.
// The clone owns no anchor names; existing definitions elsewhere stay intact.
func cloneAgentHookYAML(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	original := node
	node = yamlAgentHookResolved(node)
	if node == nil {
		return nil
	}
	clone := *node
	clone.Anchor = ""
	clone.Alias = nil
	clone.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		clonedChild := cloneAgentHookYAML(child)
		if clonedChild == nil {
			return nil
		}
		clone.Content[i] = clonedChild
	}
	if original.Kind == yaml.AliasNode {
		if original.HeadComment != "" {
			clone.HeadComment = original.HeadComment
		}
		if original.LineComment != "" {
			clone.LineComment = original.LineComment
		}
		if original.FootComment != "" {
			clone.FootComment = original.FootComment
		}
	}
	return &clone
}

func walkYAMLAgentHookScalars(node *yaml.Node, replace func(string) string) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		node.Value = replace(node.Value)
	}
	for _, child := range node.Content {
		walkYAMLAgentHookScalars(child, replace)
	}
}

func writeOwnedAgentHookPlan(plan ownedAgentHookPlan) (agenthook.Result, error) {
	result := plan.result
	if !result.Changed {
		return result, nil
	}
	staged, err := stageOwnedAgentHookPlan(plan)
	if err != nil {
		return result, err
	}
	return publishOwnedAgentHookPlan(plan, staged)
}

func stageOwnedAgentHookPlan(plan ownedAgentHookPlan) (*atomicfile.File, error) {
	result := plan.result
	options := []atomicfile.Option{atomicfile.WithPerm(0o600), atomicfile.WithPreserveMode()}
	writePath := result.ConfigPath
	info, err := os.Lstat(writePath)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		options = append(options, atomicfile.WithFollowLink())
		writePath, err = pathresolve.EvalSymlinks(writePath)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return nil, err
	}
	staged, err := atomicfile.Create(result.ConfigPath, options...)
	if err != nil {
		return nil, err
	}
	if _, err := staged.Write(result.Data); err != nil {
		return nil, errors.Join(fmt.Errorf("atomicfile: write %s: %w", result.ConfigPath, err), staged.Abort())
	}
	return staged, nil
}

func publishOwnedAgentHookPlan(plan ownedAgentHookPlan, staged *atomicfile.File) (result agenthook.Result, err error) {
	result = plan.result
	if staged == nil {
		return result, errors.New("missing staged agent hook config")
	}
	defer func() { err = errors.Join(err, staged.Abort()) }()
	if err := verifyOwnedAgentHookPlanSnapshot(plan); err != nil {
		return result, err
	}
	return result, staged.Commit()
}

func verifyOwnedAgentHookPlanSnapshot(plan ownedAgentHookPlan) error {
	current, err := os.ReadFile(plan.result.ConfigPath) //nolint:gosec // G304: selected agent hook config target from the plan.
	currentExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read agent hook config %q before writing: %w", plan.result.ConfigPath, err)
	}
	if currentExists != plan.originalExists || !bytes.Equal(current, plan.original) {
		return fmt.Errorf("agent hook config %q changed after planning; rerun the command", plan.result.ConfigPath)
	}
	return nil
}

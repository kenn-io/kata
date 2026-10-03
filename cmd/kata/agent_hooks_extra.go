package main

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kit/agenthook"
)

// Extra command providers use native schemas rather than extending Kit's older
// Droid profile. They only construct plans; the shared publisher owns writes.
func planExtraAgentHooks(opts nativeAgentHookOptions, remove bool) (nativeAgentHookPlan, error) {
	if opts.SourceSet {
		return nativeAgentHookPlan{}, errors.New("--source customization is unsupported for native command-hook installers; authored source commands are preserved")
	}
	if opts.Scope == "" {
		opts.Scope = "user"
	}
	if opts.Scope != "user" && opts.Scope != "project" {
		return nativeAgentHookPlan{}, errors.New("scope must be user or project")
	}
	if opts.ManagedAttention && opts.Agent != "muse" {
		return nativeAgentHookPlan{}, errors.New("--managed-attention is supported only for Muse")
	}
	if opts.Home == "" {
		var err error
		opts.Home, err = os.UserHomeDir()
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
	}
	if opts.Scope == "project" && slices.Contains([]string{"kimi-code", "kimi", "zcode"}, opts.Agent) {
		return nativeAgentHookPlan{}, fmt.Errorf("%s project hook discovery is unverified; use user scope with --config for an explicit native file", opts.Agent)
	}
	if !remove && (opts.Agent == "kimi" || opts.Agent == "grok") && !opts.Attention {
		return nativeAgentHookPlan{}, fmt.Errorf("%s does not consume hook contract context; use --attention for native lifecycle hooks and AGENTS.md for the contract", opts.Agent)
	}
	if opts.Agent == "muse" {
		return planMuseAgentHooks(opts, remove)
	}
	path, err := extraAgentHookPath(opts)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	if opts.Agent == "droid" {
		return planDroidAgentHooks(opts, remove, path)
	}
	if opts.Agent == "kimi-code" || opts.Agent == "kimi" {
		return planExtraTOMLHooks(opts, remove, path)
	}
	data, exists, err := readNativeAgentHookFile(path)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	plan, content, err := planExtraJSONHooks(opts, remove, path, data, exists, false)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	plan.Changes = []nativeAgentHookChange{{Path: path, Original: data, OriginalExists: exists, Content: content, Remove: remove && !exists}}
	return plan, nil
}

func extraAgentHookPath(opts nativeAgentHookOptions) (string, error) {
	if opts.ConfigPath != "" {
		return filepath.Abs(opts.ConfigPath)
	}
	base := opts.Home
	actualHome, homeErr := os.UserHomeDir()
	if opts.Scope == "user" && homeErr == nil && filepath.Clean(opts.Home) == filepath.Clean(actualHome) {
		var variable string
		var suffix []string
		switch opts.Agent {
		case "kimi-code":
			variable = "KIMI_CODE_HOME"
			suffix = []string{"config.toml"}
		case "grok":
			variable = "GROK_HOME"
			suffix = []string{"hooks", "kata.json"}
		case "muse":
			variable = "XDG_CONFIG_HOME"
			suffix = []string{"muse", "settings.json"}
		}
		if variable != "" {
			if root := os.Getenv(variable); root != "" {
				return filepath.Abs(filepath.Join(append([]string{root}, suffix...)...))
			}
		}
	}
	if opts.Scope == "project" {
		base = opts.Dir
		if base == "" {
			return "", errors.New("project scope requires a workspace directory")
		}
	}
	var parts []string
	switch opts.Agent {
	case "droid":
		parts = []string{".factory", "hooks.json"}
	case "antigravity":
		if opts.Scope == "project" {
			parts = []string{".agents", "hooks.json"}
		} else {
			parts = []string{".gemini", "config", "hooks.json"}
		}
	case "zcode":
		parts = []string{".zcode", "cli", "config.json"}
	case "kimi-code":
		parts = []string{".kimi-code", "config.toml"}
	case "kimi":
		parts = []string{".kimi", "config.toml"}
	case "grok":
		parts = []string{".grok", "hooks", "kata.json"}
	case "muse":
		if opts.Scope == "project" {
			parts = []string{".muse", "hooks.json"}
		} else {
			parts = []string{".config", "muse", "settings.json"}
		}
	default:
		return "", fmt.Errorf("unknown native command-hook target %q", opts.Agent)
	}
	return filepath.Abs(filepath.Join(append([]string{base}, parts...)...))
}

func writeExtraAgentContract(target string, input io.Reader, output io.Writer, text string) error {
	if target == "kimi" || target == "grok" {
		return fmt.Errorf("%s runtime discards successful hook context; contract hooks are unavailable", target)
	}
	const limit = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("native hook payload too large")
	}
	var payload map[string]any
	if err = json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if payload == nil {
		return errors.New("native hook payload must be an object")
	}
	requiredString := func(key string) error {
		if value, ok := payload[key].(string); !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("native hook requires %s", key)
		}
		return nil
	}
	var response any
	switch target {
	case "droid", "muse", "zcode", "kimi-code":
		eventKey, idKey, event := "hook_event_name", "session_id", "SessionStart"
		if target == "zcode" {
			eventKey, idKey = "hookEventName", "sessionId"
		}
		if target == "kimi-code" {
			event = "UserPromptSubmit"
		}
		if payload[eventKey] != event {
			return fmt.Errorf("%s contract requires native %s", target, event)
		}
		if err := requiredString(idKey); err != nil {
			return err
		}
		if err := requiredString("cwd"); err != nil {
			return err
		}
		if target == "kimi-code" {
			if _, ok := payload["prompt"].(string); !ok {
				return errors.New("Kimi Code UserPromptSubmit requires prompt text") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
			}
			_, err = io.WriteString(output, text+"\n")
			return err
		}
		response = map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": text}}
	case "antigravity":
		if err := requiredString("conversationId"); err != nil {
			return err
		}
		paths, ok := payload["workspacePaths"].([]any)
		if !ok {
			return errors.New("Antigravity requires workspacePaths") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
		}
		for _, path := range paths {
			if _, ok := path.(string); !ok {
				return errors.New("Antigravity workspacePaths must contain strings") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
			}
		}
		var invocation struct {
			Num *int `json:"invocationNum"`
		}
		if err := json.Unmarshal(data, &invocation); err != nil {
			return err
		}
		if invocation.Num == nil || *invocation.Num < 0 {
			return errors.New("Antigravity requires a nonnegative invocationNum") //nolint:staticcheck // ST1005: preserve the native product name in this user-facing payload error.
		}
		response = map[string]any{}
		if *invocation.Num == 0 {
			response = map[string]any{"injectSteps": []any{map[string]any{"ephemeralMessage": text}}}
		}
	default:
		return fmt.Errorf("unsupported contract target %q", target)
	}
	encoded, err := json.Marshal(response, json.Deterministic(true))
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = output.Write(encoded)
	return err
}

func extraHookCapabilities(target string) (contract, start, end bool) {
	switch target {
	case "droid", "kimi-code", "muse":
		return true, true, true
	case "antigravity":
		return true, false, false
	case "zcode":
		return true, true, false
	case "kimi", "grok":
		return false, true, true
	}
	return false, false, false
}

func extraHookDesired(opts nativeAgentHookOptions, remove bool, current [3]bool) [3]bool {
	desired := current
	contract, start, end := extraHookCapabilities(opts.Agent)
	desired[0], desired[1], desired[2] = current[0] && contract, current[1] && start, current[2] && end
	if remove {
		if opts.Contract {
			desired[0] = false
		}
		if opts.Attention {
			desired[1], desired[2] = false, false
		}
	} else {
		if opts.Contract && contract {
			desired[0] = true
		}
		if opts.Attention {
			desired[1], desired[2] = start, end
		}
	}
	return desired
}

func extraHookPlan(path string, current, desired [3]bool, target string) nativeAgentHookPlan {
	plan := nativeAgentHookPlan{Path: path, CurrentContract: current[0], CurrentAttentionStart: current[1], CurrentAttentionEnd: current[2], Contract: desired[0], AttentionStart: desired[1], AttentionEnd: desired[2]}
	switch target {
	case "antigravity":
		plan.Warnings = []string{"Antigravity has no native session start/end lifecycle; use the Kata launcher for attention"}
	case "zcode":
		plan.Warnings = []string{"ZCode has no native SessionEnd; use the Kata launcher for terminal attention cleanup"}
	case "kimi":
		plan.Warnings = []string{"Kimi CLI discards successful hook context; supply the contract through AGENTS.md"}
	case "grok":
		plan.Warnings = []string{"Grok discards successful session/prompt hook context; use AGENTS.md. Grok also scans Claude/Cursor compatibility hooks by default; disable overlapping registrations to avoid duplicate attention"}
	}
	return plan
}

func extraHookEvent(target string, kind agentHookKind) string {
	if kind == attentionEndHook {
		return "SessionEnd"
	}
	if kind == contractHook && target == "kimi-code" {
		return "UserPromptSubmit"
	}
	if target == "antigravity" {
		return "PreInvocation"
	}
	return "SessionStart"
}

func extraHookArguments(target string, kind agentHookKind) []string {
	if kind == contractHook {
		return []string{"agent-hook", "contract", target}
	}
	return []string{"agent-hook", "attention-native", target, string(kind)}
}

func extraHookHandler(opts nativeAgentHookOptions, kind agentHookKind) (map[string]any, error) {
	args := agentHookOwnershipArgs(opts.Executable, extraHookArguments(opts.Agent, kind), kind)
	if opts.Agent == "zcode" {
		if strings.TrimSpace(opts.Executable) == "" {
			return nil, errors.New("hook executable is required")
		}
		return map[string]any{"type": "process", "command": opts.Executable, "args": args, "timeoutMs": 10000}, nil
	}
	commands, err := agenthook.BuildCommand(opts.Executable, args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "command", "command": commands.Native, "timeout": 10}, nil
}

// Custom source arguments, shell wrappers, platform disagreement and authored
// argv are foreign. Ownership never follows a substring or arbitrary marker.
var extraHookCommandFields = []string{"command", "commandWindows", "command_windows", "bash", "powershell"}

func extraHookKind(target string, handler map[string]any) agentHookKind {
	if target == "zcode" && handler["type"] == "process" {
		command, ok := handler["command"].(string)
		if !ok {
			return ""
		}
		raw, ok := handler["args"].([]any)
		if !ok {
			return ""
		}
		argv := []string{command}
		for _, arg := range raw {
			value, ok := arg.(string)
			if !ok {
				return ""
			}
			argv = append(argv, value)
		}
		for _, field := range []string{"commandWindows", "command_windows", "bash", "powershell"} {
			if value, ok := handler[field]; ok && value != "" {
				return ""
			}
		}
		return extraHookArgvKind(target, argv)
	}
	if value, ok := handler["type"]; ok && value != "command" {
		return ""
	}
	if raw, ok := handler["args"]; ok {
		args, ok := raw.([]any)
		if !ok || len(args) != 0 {
			return ""
		}
	}
	var kind agentHookKind
	for _, field := range extraHookCommandFields {
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
		powershell := field == "powershell"
		if powershell {
			command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "& "))
		}
		argv, ok := literalAgentHookWords(command, powershell, field == "commandWindows" || field == "command_windows" || (field == "command" && runtime.GOOS == "windows"))
		if !ok {
			return ""
		}
		candidate := extraHookArgvKind(target, argv)
		if candidate == "" || (kind != "" && kind != candidate) {
			return ""
		}
		kind = candidate
	}
	return kind
}

func extraHookArgvKind(target string, argv []string) agentHookKind {
	if len(argv) < 2 {
		return ""
	}
	executable := argv[0]
	if executable == "" || (strings.Contains(executable, "=") && !strings.ContainsAny(executable, "/\\")) {
		return ""
	}
	if !isKataHookExecutable(executable) && !strings.ContainsAny(executable, "/\\") {
		return ""
	}
	rest := canonicalAgentHookArgs(argv[1:])
	for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
		args := extraHookArguments(target, kind)
		if isKataHookExecutable(executable) && slices.Equal(rest, args) {
			return kind
		}
		source := legacyAgentContractHookSource
		if kind != contractHook {
			source = legacyAttentionHookSource + string(kind)
		}
		if slices.Equal(rest, append(args, "--source", source)) {
			return kind
		}
	}
	return ""
}

func extraKindIndex(kind agentHookKind) int {
	switch kind {
	case contractHook:
		return 0
	case attentionStartHook:
		return 1
	case attentionEndHook:
		return 2
	}
	return -1
}

// Lossless rendering keeps the order and raw bytes of every unchanged JSON
// subtree. Native JSON rejects duplicate keys and comments before any plan.
func renderExtraJSON(original []byte, value any) ([]byte, error) {
	if len(bytes.TrimSpace(original)) == 0 {
		return marshalAgentHookJSON(value)
	}
	var old any
	if err := json.Unmarshal(original, &old, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, err
	}
	if reflect.DeepEqual(old, value) {
		return bytes.Clone(original), nil
	}
	data, err := renderExtraJSONValue(bytes.TrimSpace(original), old, value)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func renderExtraJSONValue(raw []byte, old, value any) ([]byte, error) {
	if reflect.DeepEqual(old, value) {
		return bytes.Clone(raw), nil
	}
	if object, ok := value.(map[string]any); ok {
		oldObject, _ := old.(map[string]any)
		var order []string
		children := map[string][]byte{}
		if len(raw) > 0 && raw[0] == '{' {
			decoder := jsontext.NewDecoder(bytes.NewReader(raw))
			if _, err := decoder.ReadToken(); err != nil {
				return nil, err
			}
			for decoder.PeekKind() != '}' {
				key, err := decoder.ReadToken()
				if err != nil {
					return nil, err
				}
				name := key.String()
				child, err := decoder.ReadValue()
				if err != nil {
					return nil, err
				}
				order = append(order, name)
				children[name] = bytes.Clone(child)
			}
		}
		var added []string
		for key := range object {
			if _, ok := children[key]; !ok {
				added = append(added, key)
			}
		}
		sort.Strings(added)
		order = append(order, added...)
		spacing, closing := extraJSONContainerSpacing(raw)
		var output bytes.Buffer
		output.WriteByte('{')
		first := true
		for _, key := range order {
			child, exists := object[key]
			if !exists {
				continue
			}
			if !first {
				output.WriteByte(',')
			}
			first = false
			output.Write(spacing)
			name, _ := json.Marshal(key)
			output.Write(name)
			output.WriteByte(':')
			if len(spacing) > 0 {
				output.WriteByte(' ')
			}
			encoded, err := renderExtraJSONValue(children[key], oldObject[key], child)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
		output.Write(closing)
		output.WriteByte('}')
		return output.Bytes(), nil
	}
	if array, ok := value.([]any); ok {
		oldArray, _ := old.([]any)
		var raws [][]byte
		if len(raw) > 0 && raw[0] == '[' {
			decoder := jsontext.NewDecoder(bytes.NewReader(raw))
			if _, err := decoder.ReadToken(); err != nil {
				return nil, err
			}
			for decoder.PeekKind() != ']' {
				child, err := decoder.ReadValue()
				if err != nil {
					return nil, err
				}
				raws = append(raws, bytes.Clone(child))
			}
		}
		spacing, closing := extraJSONContainerSpacing(raw)
		var output bytes.Buffer
		output.WriteByte('[')
		for i, child := range array {
			if i > 0 {
				output.WriteByte(',')
			}
			output.Write(spacing)
			var oldChild any
			var oldRaw []byte
			for j, candidate := range oldArray {
				if raws != nil && j < len(raws) && reflect.DeepEqual(candidate, child) {
					oldChild = candidate
					oldRaw = raws[j]
					break
				}
			}
			encoded, err := renderExtraJSONValue(oldRaw, oldChild, child)
			if err != nil {
				return nil, err
			}
			output.Write(encoded)
		}
		output.Write(closing)
		output.WriteByte(']')
		return output.Bytes(), nil
	}
	return json.Marshal(value, json.Deterministic(true))
}

// Reuse the container's own indentation without reformatting unchanged child
// values, which can deliberately use different spacing or numeric literals.
func extraJSONContainerSpacing(raw []byte) (spacing, closing []byte) {
	if len(raw) < 2 || (raw[0] != '{' && raw[0] != '[') {
		return nil, nil
	}
	inner := raw[1 : len(raw)-1]
	spacing = inner[:len(inner)-len(bytes.TrimLeft(inner, " \t\r\n"))]
	closing = inner[len(bytes.TrimRight(inner, " \t\r\n")):]
	return spacing, closing
}

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// `kata init --with-hooks` installs the attention hooks from
// docs/operations/agent-orchestration.md ("Keep attention truthful with
// hooks") into a Claude Code workspace: a managed hook script plus the
// .claude/settings.json wiring that runs it at session start, after Bash
// tool calls (to spot `kata claim`), and at session stop.

//go:embed kata_attention_hook.sh
var claudeHookScript string

// claudeHookScriptName is the managed script's filename under .claude/hooks.
const claudeHookScriptName = "kata-attention.sh"

// claudeHookCommand renders the settings.json command line for one hook mode.
// $CLAUDE_PROJECT_DIR keeps the wiring correct regardless of the cwd Claude
// Code runs hooks from.
func claudeHookCommand(mode string) string {
	return `"$CLAUDE_PROJECT_DIR"/.claude/hooks/` + claudeHookScriptName + " " + mode
}

// claudeHookSpec is one desired settings.json wiring: an event, an optional
// tool matcher, and the command to run.
type claudeHookSpec struct {
	Event   string
	Matcher string
	Command string
}

// claudeHookSpecs returns the three wirings --with-hooks manages.
func claudeHookSpecs() []claudeHookSpec {
	return []claudeHookSpec{
		{Event: "SessionStart", Command: claudeHookCommand("start")},
		{Event: "PostToolUse", Matcher: "Bash", Command: claudeHookCommand("claim")},
		{Event: "Stop", Command: claudeHookCommand("stop")},
	}
}

// applyClaudeHooks is the entry point for `--with-hooks`. It writes the
// managed hook script under <dir>/.claude/hooks and merges the wiring into
// <dir>/.claude/settings.json, preserving everything else the file holds.
// Returns whether anything changed; re-running on an installed workspace is
// a no-op.
func applyClaudeHooks(dir string) (bool, error) {
	scriptChanged, err := ensureClaudeHookScript(
		filepath.Join(dir, ".claude", "hooks", claudeHookScriptName))
	if err != nil {
		return false, err
	}
	settingsChanged, err := ensureClaudeSettingsHooks(
		filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		return scriptChanged, err
	}
	return scriptChanged || settingsChanged, nil
}

// ensureClaudeHookScript writes the managed script, creating parent
// directories as needed and refreshing the file when its content drifts.
// The script is wholly kata-owned, so drift is overwritten rather than
// merged. Symlinks are refused, matching the guidance-file posture.
func ensureClaudeHookScript(path string) (bool, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing to manage symlinked %s", path)
	}
	current, exists, err := readIfExists(path)
	if err != nil {
		return false, err
	}
	if exists && current == claudeHookScript {
		return ensureExecutable(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(claudeHookScript), 0o755); err != nil { //nolint:gosec // hook script must be executable
		return false, err
	}
	if exists {
		// WriteFile's permission argument only applies at creation; a
		// pre-existing drifted file keeps its old mode unless fixed here.
		if _, err := ensureExecutable(path); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ensureExecutable restores the execute bits on the managed script when a
// pre-existing copy lost them — content-identical but 0644 still means the
// wired hooks never run. The owner bit specifically is what counts: Unix
// checks only the owner class for the owning user, so group/other execute
// bits alone leave the script unrunnable. Reports whether the mode changed.
// No-op on Windows, which has no Unix execute bits.
func ensureExecutable(path string) (bool, error) {
	if runtime.GOOS == "windows" {
		return false, nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if fi.Mode()&0o100 != 0 {
		return false, nil
	}
	if err := os.Chmod(path, 0o755); err != nil { //nolint:gosec // hook script must be executable
		return false, err
	}
	return true, nil
}

// ensureClaudeSettingsHooks merges the hook wiring into settings.json. The
// file is user-owned, so the merge is additive: unknown keys and existing
// hook entries are preserved verbatim (modulo re-encoding), and each kata
// wiring is appended only when its command is not already present under its
// event. A file that fails to parse is left untouched and reported.
func ensureClaudeSettingsHooks(path string) (bool, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("refusing to manage symlinked %s", path)
	}
	content, exists, err := readIfExists(path)
	if err != nil {
		return false, err
	}
	settings := map[string]any{}
	if exists {
		if err := json.Unmarshal([]byte(content), &settings); err != nil {
			return false, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", path, err)
		}
	}

	changed := false
	for _, spec := range claudeHookSpecs() {
		c, err := upsertClaudeHook(settings, spec)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		changed = changed || c
	}
	if exists && !changed {
		return false, nil
	}

	encoded, err := encodeClaudeSettings(settings)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, err
	}
	if exists {
		err = os.WriteFile(path, encoded, 0o644) //nolint:gosec // settings.json is committed config
	} else {
		err = writeNewGuidanceFile(path, encoded)
		if errors.Is(err, os.ErrExist) {
			// Something raced the file into place between read and write;
			// surface it rather than overwriting what appeared.
			err = fmt.Errorf("refusing to overwrite %s: %w", path, err)
		}
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// upsertClaudeHook appends spec's wiring under its event unless a hook with
// the same command is already wired there. Existing structures with
// unexpected shapes are an error rather than a guessed edit.
func upsertClaudeHook(settings map[string]any, spec claudeHookSpec) (bool, error) {
	hooks, err := ensureObject(settings, "hooks")
	if err != nil {
		return false, err
	}
	groups, ok := hooks[spec.Event].([]any)
	if !ok && hooks[spec.Event] != nil {
		return false, fmt.Errorf("hooks.%s has an unexpected shape", spec.Event)
	}
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if em["command"] == spec.Command {
				return false, nil
			}
		}
	}
	group := map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": spec.Command}},
	}
	if spec.Matcher != "" {
		group["matcher"] = spec.Matcher
	}
	hooks[spec.Event] = append(groups, group)
	return true, nil
}

// ensureObject returns m[key] as an object, creating it when absent.
func ensureObject(m map[string]any, key string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		obj := map[string]any{}
		m[key] = obj
		return obj, nil
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q has an unexpected shape", key)
	}
	return obj, nil
}

// encodeClaudeSettings renders settings as 2-space-indented JSON without
// HTML escaping, trailing newline included — the conventional settings.json
// shape. Object keys come out sorted; that is the one formatting liberty the
// merge takes with a user-owned file.
func encodeClaudeSettings(settings map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

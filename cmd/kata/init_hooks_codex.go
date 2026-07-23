package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/BurntSushi/toml"
)

// `kata init --with-codex-hooks` wires the work.attention harness into a Codex
// CLI workspace by writing .codex/hooks.json. It installs a single SessionStart
// command hook that runs `kata attention-hook start`, the same hidden
// subcommand the Claude Code wiring calls.
//
// Codex executes a command hook's `command` string through a login shell
// ($SHELL -lc), so the whole invocation is one string rather than Claude
// Code's separate command+args exec form; `kata` resolves via PATH exactly as
// an interactive shell would. Only the "start" mode is installed: Codex at the
// pinned release has no session-end hook event, so an end wiring would be inert.
//
// Security boundary — the bare `kata` command is deliberate, not an oversight
// (reviews keep flagging it as a PATH-impersonation risk). Pinning an absolute
// path at install time would make hooks.json machine-specific, and the file
// lives in the workspace where it may be committed or shared; the shipped
// Claude Code wiring makes the same choice (bare "kata" in .claude/settings.json).
// A PATH that resolves `kata` to a repository-controlled binary means the
// developer's shell is already compromised — every make/npm/git invocation has
// the same exposure — so a hostile PATH is outside this installer's threat
// model.

// codexHookHandler renders the managed Codex command-hook entry. timeout is in
// seconds and is stored as a json.Number so a re-run's decoded file compares
// equal to this desired handler (the merge is DeepEqual-based).
func codexHookHandler() map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata attention-hook start",
		"timeout": json.Number("10"),
	}
}

// applyCodexHooks is the entry point for `--with-codex-hooks`. It merges the
// SessionStart wiring into <dir>/.codex/hooks.json, preserving everything else
// the file holds, and returns whether anything changed plus any best-effort
// warnings for the caller to surface. Re-running on an installed workspace is a
// no-op.
//
// All filesystem steps run fd-relative to the workspace root through os.Root,
// so a component swapped for a symlink between validation and use cannot
// redirect a read or write outside the workspace. Pre-existing symlinked
// .codex or hooks.json components are refused outright, matching the Claude
// wiring's posture.
func applyCodexHooks(dir string) (bool, []string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".codex", ".codex/hooks.json"); err != nil {
		return false, nil, err
	}
	warnings := codexConfigHooksWarnings(root)
	changed, err := ensureCodexHooksFile(root)
	if err != nil {
		return false, nil, err
	}
	return changed, warnings, nil
}

// ensureCodexHooksFile merges the managed SessionStart hook into hooks.json
// (all I/O relative to the workspace root). The file is user-owned, so the
// merge is additive: unknown keys (including "description") and existing hook
// groups are preserved verbatim modulo re-encoding, and the managed group is
// appended only when its exact handler is not already present in a matcher-less
// SessionStart group. A file that fails to parse is left untouched and
// reported.
func ensureCodexHooksFile(root *os.Root) (bool, error) {
	const rel = ".codex/hooks.json"
	path := filepath.Join(root.Name(), rel) // display only
	content, err := root.ReadFile(rel)
	exists := true
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		exists = false
	}
	file := map[string]any{}
	if exists {
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&file); err != nil {
			return false, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", path, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = errors.New("multiple JSON values")
			}
			return false, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", path, err)
		}
		// `null` is valid JSON and decodes to a nil map, which the merge below
		// would panic assigning into.
		if file == nil {
			return false, fmt.Errorf("parse %s: hooks.json root is not an object (fix or remove it, then re-run)", path)
		}
	}

	changed, err := upsertCodexHook(file)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if exists && !changed {
		return false, nil
	}

	encoded, err := encodeClaudeSettings(file)
	if err != nil {
		return false, err
	}
	if err := root.MkdirAll(".codex", 0o750); err != nil {
		return false, err
	}
	if exists {
		err = atomicReplaceSettings(root, rel, encoded)
	} else {
		// O_EXCL: something racing the file into place between read and write
		// is surfaced rather than overwritten.
		var f *os.File
		f, err = root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, werr := f.Write(encoded)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			err = werr
		} else if errors.Is(err, os.ErrExist) {
			err = fmt.Errorf("refusing to overwrite %s: %w", path, err)
		}
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// upsertCodexHook appends the managed SessionStart command group unless the
// exact managed handler is already present in a matcher-less SessionStart
// group. Existing groups are otherwise preserved, including matcher-scoped
// groups that cannot be interpreted as the managed wiring.
func upsertCodexHook(file map[string]any) (bool, error) {
	hooks, err := ensureObject(file, "hooks")
	if err != nil {
		return false, err
	}
	const event = "SessionStart"
	var groups []any
	if raw, exists := hooks[event]; exists {
		var ok bool
		groups, ok = raw.([]any)
		if !ok {
			return false, fmt.Errorf("hooks.%s has an unexpected shape", event)
		}
	}
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		// The managed group carries no matcher; a matcher-scoped group cannot
		// stand in for it, so skip those.
		if _, hasMatcher := gm["matcher"]; hasMatcher {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			if reflect.DeepEqual(e, codexHookHandler()) {
				return false, nil
			}
		}
	}
	hooks[event] = append(groups, map[string]any{
		"hooks": []any{codexHookHandler()},
	})
	return true, nil
}

// codexConfigHooksWarnings returns a best-effort warning when
// .codex/config.toml already defines a [hooks] table. Codex loads hooks from
// both config.toml and hooks.json, so an operator with pre-existing TOML hooks
// should know kata's hooks.json now runs alongside them. Detection is a real
// TOML parse (BurntSushi/toml, already a dependency) for a top-level `hooks`
// key. A missing, symlinked, unreadable, or unparseable config.toml yields no
// warning — config.toml is Codex's own file and the hooks.json install is what
// this command guarantees.
func codexConfigHooksWarnings(root *os.Root) []string {
	const rel = ".codex/config.toml"
	content, err := root.ReadFile(rel)
	if err != nil {
		// Missing is the common case; a symlinked or otherwise unreadable
		// config.toml is not this installer's file to police, so stay silent.
		return nil
	}
	var parsed map[string]any
	if _, err := toml.Decode(string(content), &parsed); err != nil {
		return nil
	}
	if _, ok := parsed["hooks"]; !ok {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s already defines a [hooks] table; kata installed its SessionStart hook into %s, which Codex loads in addition to config.toml hooks",
		filepath.Join(root.Name(), rel),
		filepath.Join(root.Name(), ".codex/hooks.json"),
	)}
}

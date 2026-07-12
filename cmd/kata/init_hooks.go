package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

// `kata init --with-hooks` wires the work.attention harness hooks from
// docs/operations/agent-orchestration.md ("Keep attention truthful with
// hooks") into a Claude Code workspace. It writes only the
// .claude/settings.json wiring: each event executes
// `kata attention-hook <mode>`, the hidden subcommand that carries all the
// hook logic in the kata binary.
//
// There is deliberately no repo-committed hook script. Claude Code re-prompts
// for approval when a settings.json hook command changes, but not when a
// separate script file's *content* changes under a stable command string — so
// a committed script would be a code-execution vector a hostile commit could
// mutate silently. Keeping the logic in the installed binary makes the
// settings.json executable and arguments the whole approval contract.

// claudeHookSpec is one desired settings.json lifecycle wiring.
type claudeHookSpec struct {
	Event   string
	Matcher string
	Mode    string
}

// claudeHookSpecs returns the two lifecycle wirings --with-hooks manages.
func claudeHookSpecs() []claudeHookSpec {
	return []claudeHookSpec{
		{Event: "SessionStart", Matcher: "startup|resume|clear", Mode: "start"},
		{Event: "SessionEnd", Matcher: "logout|prompt_input_exit|bypass_permissions_disabled|other", Mode: "end"},
	}
}

// claudeHookHandler renders Claude Code's exec form, keeping the executable
// and arguments separate so no shell parses the command.
func claudeHookHandler(mode string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata",
		"args":    []any{"attention-hook", mode},
	}
}

// applyClaudeHooks is the entry point for `--with-hooks`. It merges the wiring
// into <dir>/.claude/settings.json, preserving everything else the file holds.
// Returns whether anything changed; re-running on an installed workspace is a
// no-op.
//
// Every filesystem step runs fd-relative to the workspace root through
// os.Root, so a component swapped for a symlink between validation and use
// (TOCTOU in a shared-writable checkout) cannot redirect a read or write
// outside the workspace. Pre-existing symlinked components — .claude or
// settings.json pointing anywhere, even inside the workspace — are refused
// outright, matching the guidance-file posture.
func applyClaudeHooks(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".claude", ".claude/settings.json"); err != nil {
		return false, err
	}
	return ensureClaudeSettingsHooks(root)
}

// refuseSymlinkComponents rejects any existing symlink among the given
// root-relative paths (each successive entry one component deeper). Paths
// that do not exist yet are fine — they will be created as real entries.
// Root.Lstat is fd-relative, so the check itself cannot be redirected by a
// concurrent rename outside the root.
func refuseSymlinkComponents(root *os.Root, rels ...string) error {
	for _, rel := range rels {
		fi, err := root.Lstat(rel)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case err != nil:
			return err
		case fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing to manage symlinked %s", filepath.Join(root.Name(), rel))
		}
	}
	return nil
}

// ensureClaudeSettingsHooks merges the hook wiring into settings.json (all
// I/O relative to the workspace root). The file is user-owned, so the merge
// is additive: unknown keys and existing hook entries are preserved verbatim
// (modulo re-encoding), and each kata wiring is appended only when its exact
// handler is not already present under the required event matcher. A file that
// fails to parse is left untouched and reported.
func ensureClaudeSettingsHooks(root *os.Root) (bool, error) {
	const rel = ".claude/settings.json"
	path := filepath.Join(root.Name(), rel) // display only
	content, err := root.ReadFile(rel)
	exists := true
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		exists = false
	}
	settings := map[string]any{}
	if exists {
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		if err := decoder.Decode(&settings); err != nil {
			return false, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", path, err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			if err == nil {
				err = errors.New("multiple JSON values")
			}
			return false, fmt.Errorf("parse %s: %w (fix or remove it, then re-run)", path, err)
		}
		// `null` is valid JSON and decodes to a nil map, which the merge
		// below would panic assigning into.
		if settings == nil {
			return false, fmt.Errorf("parse %s: settings root is not an object (fix or remove it, then re-run)", path)
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
	if err := root.MkdirAll(".claude", 0o750); err != nil {
		return false, err
	}
	if exists {
		err = atomicReplaceSettings(root, rel, encoded)
	} else {
		// O_EXCL: something racing the file into place between read and
		// write is surfaced rather than overwritten.
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

// atomicReplaceSettings rewrites an existing settings.json without ever
// exposing a partial document. The merged JSON is staged in a sibling temp
// file in .claude, fully written, fsynced, and closed; only then is it
// renamed over rel. A rename within a directory is atomic, so a reader always
// observes either the old file or the whole new one — an interrupted or short
// write can only truncate the throwaway temp, never the user's live config.
// The existing file's permissions are carried onto the replacement so a
// tightened mode is not silently reset to the default.
func atomicReplaceSettings(root *os.Root, rel string, encoded []byte) error {
	perm := os.FileMode(0o644)
	if fi, err := root.Stat(rel); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp, f, err := createSettingsTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	// Any exit before the rename lands must not leave the temp behind.
	renamed := false
	defer func() {
		if !renamed {
			_ = root.Remove(tmp)
		}
	}()
	writeErr := func() error {
		if _, err := f.Write(encoded); err != nil {
			return err
		}
		if err := f.Chmod(perm); err != nil {
			return err
		}
		return f.Sync()
	}()
	if cerr := f.Close(); writeErr == nil {
		writeErr = cerr
	}
	if writeErr != nil {
		return writeErr
	}
	// os.Root.Rename replaces an existing destination on every supported OS:
	// on Unix it is renameat(2), and on Windows it is renameat via
	// windows.Renameat, which sets FILE_RENAME_REPLACE_IF_EXISTS. The export
	// path's separate MoveFileEx helper is not needed here — routing the
	// rename through the *os.Root keeps it fd-relative so the atomic swap
	// cannot be redirected outside the workspace by a concurrent symlink.
	if err := root.Rename(tmp, rel); err != nil {
		return err
	}
	renamed = true
	return nil
}

// createSettingsTemp opens a fresh, exclusively-created temp file for staging
// settings.json inside dir (root-relative). O_EXCL with a random name means a
// concurrent installer gets its own staging file rather than a shared one, and
// the create cannot be redirected through a pre-planted symlink.
func createSettingsTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(dir, "settings.json."+hex.EncodeToString(b[:])+".tmp")
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return name, f, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, err
		}
	}
	return "", nil, fmt.Errorf("could not create a staging temp file in %s", filepath.Join(root.Name(), dir))
}

// upsertClaudeHook appends spec's wiring under its event unless the exact
// managed handler is already present in a group with the exact desired scope.
// Existing groups are otherwise preserved, including groups with wildcard or
// malformed matchers that cannot be interpreted as the managed wiring.
func upsertClaudeHook(settings map[string]any, spec claudeHookSpec) (bool, error) {
	hooks, err := ensureObject(settings, "hooks")
	if err != nil {
		return false, err
	}
	var groups []any
	if rawGroups, exists := hooks[spec.Event]; exists {
		var ok bool
		groups, ok = rawGroups.([]any)
		if !ok {
			return false, fmt.Errorf("hooks.%s has an unexpected shape", spec.Event)
		}
	}
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		rawMatcher, hasMatcher := gm["matcher"]
		if spec.Matcher == "" {
			if hasMatcher {
				continue
			}
		} else {
			matcher, ok := rawMatcher.(string)
			if !ok || matcher != spec.Matcher {
				continue
			}
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			if reflect.DeepEqual(e, claudeHookHandler(spec.Mode)) {
				return false, nil
			}
		}
	}
	group := map[string]any{
		"hooks": []any{claudeHookHandler(spec.Mode)},
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
	if !ok {
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

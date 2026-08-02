package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/agenthook"
)

// `kata init --with-hooks` wires the work.attention lifecycle into a Claude
// Code workspace. The commands call the installed kata binary directly; no
// repository-controlled hook script can change independently of the command a
// user approved.

type claudeHookSpec struct {
	event   agenthook.Event
	matcher string
	mode    string
}

func claudeHookSpecs() []claudeHookSpec {
	return []claudeHookSpec{
		{
			event:   agenthook.EventSessionStart,
			matcher: "startup|resume|clear",
			mode:    "start",
		},
		{
			event:   agenthook.EventSessionEnd,
			matcher: "logout|prompt_input_exit|bypass_permissions_disabled|other",
			mode:    "end",
		},
	}
}

func applyClaudeHooks(dir string) (bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if err := refuseSymlinkComponents(root, ".claude", ".claude/settings.json"); err != nil {
		return false, err
	}
	const rel = ".claude/settings.json"
	configPath := filepath.Join(root.Name(), rel)
	if err := validateManagedHookJSON(
		configPath,
		agenthook.EventSessionStart,
		agenthook.EventSessionEnd,
	); err != nil {
		return false, err
	}
	registrations := make([]attentionHookRegistration, 0, len(claudeHookSpecs()))
	for _, spec := range claudeHookSpecs() {
		registrations = append(registrations, attentionHookRegistration{
			mode: spec.mode,
			hook: agenthook.Hook{Event: spec.event, Matcher: spec.matcher},
		})
	}
	return installAttentionHooks(
		root,
		rel,
		agenthook.AgentClaude,
		migrateLegacyClaudeHooks,
		registrations...,
	)
}

// migrateLegacyClaudeHooks removes only the exact command-plus-arguments
// handlers shipped in kata v0.13.0. It is retained through v0.15.0 and removed
// by issue m7d5; all normal ownership is marker-based in kit.
func migrateLegacyClaudeHooks(settings map[string]any) (bool, error) {
	changed := false
	for _, spec := range claudeHookSpecs() {
		removed, err := removeExactAgentHook(
			settings,
			spec.event,
			spec.matcher,
			map[string]any{
				"type":    "command",
				"command": "kata",
				"args":    []any{"attention-hook", spec.mode},
			},
		)
		if err != nil {
			return false, err
		}
		changed = changed || removed
	}
	return changed, nil
}

// atomicReplaceSettings stages a complete config in the workspace directory,
// preserves its mode, and atomically renames it over the existing file.
func atomicReplaceSettings(root *os.Root, rel string, encoded []byte) error {
	perm := os.FileMode(0o644)
	if fi, err := root.Stat(rel); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp, f, err := createSettingsTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
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
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if err := root.Rename(tmp, rel); err != nil {
		return err
	}
	renamed = true
	return nil
}

// atomicCreateSettings publishes a fully written sibling staging file with a
// no-overwrite hard link. A concurrent creator wins cleanly; no reader can
// observe a partial config at rel.
func atomicCreateSettings(root *os.Root, rel string, encoded []byte) error {
	tmp, f, err := createSettingsTemp(root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	writeErr := func() error {
		if _, err := f.Write(encoded); err != nil {
			return err
		}
		if err := f.Chmod(0o644); err != nil {
			return err
		}
		return f.Sync()
	}()
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if err := publishNewAgentHookConfig(root, tmp, rel); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite %s: %w", filepath.Join(root.Name(), rel), err)
		}
		return err
	}
	return nil
}

func createSettingsTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range 10 {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, err
		}
		name := filepath.Join(dir, "hooks.json."+hex.EncodeToString(random[:])+".tmp")
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

func encodeClaudeSettings(settings map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(settings); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

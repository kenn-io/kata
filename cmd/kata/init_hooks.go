package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/agenthook"
)

// `kata init --with-hooks` wires the work.attention lifecycle into a Claude
// Code workspace. Kit owns config parsing, hook ownership, and file updates.

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

	configPath := filepath.Join(root.Name(), ".claude", "settings.json")
	legacy := make([]legacyAgentHook, 0, len(claudeHookSpecs()))
	for _, spec := range claudeHookSpecs() {
		legacy = append(legacy, legacyAgentHook{
			event:   spec.event,
			matcher: spec.matcher,
			handlers: []map[string]any{
				{
					"type":    "command",
					"command": "kata",
					"args":    []any{"attention-hook", spec.mode},
				},
				{
					"type":    "command",
					"command": "kata attention-hook " + spec.mode,
				},
			},
		})
	}
	changed, err := migrateLegacyAgentHooks(configPath, legacy)
	if err != nil {
		return false, err
	}
	for _, spec := range claudeHookSpecs() {
		result, err := agenthook.Install(agenthook.AgentClaude, agenthook.InstallOptions{
			ConfigPath: configPath,
			Executable: "kata",
			Arguments: []string{
				"attention-hook", spec.mode, "--source", attentionHookSource + spec.mode,
			},
			Marker: "--source " + attentionHookSource + spec.mode,
			Hooks: []agenthook.Hook{{
				Event:   spec.event,
				Matcher: spec.matcher,
			}},
		})
		if err != nil {
			return false, err
		}
		changed = changed || result.Changed
	}
	return changed, nil
}

// refuseSymlinkComponents preserves kata's workspace boundary. Kit owns every
// other config-file concern.
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

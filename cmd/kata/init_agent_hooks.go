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

	"go.kenn.io/kit/agenthook"
)

type attentionHookRegistration struct {
	mode string
	hook agenthook.Hook
}

type agentHookConfigMigration func(map[string]any) (bool, error)

var installAgentHookConfig = agenthook.Install
var publishNewAgentHookConfig = func(root *os.Root, staging, rel string) error {
	return root.Link(staging, rel)
}

// installAttentionHooks builds the complete final config in a private staging
// file using kit, then publishes it to the workspace in one atomic write. This
// keeps multi-command installs and legacy adoption from exposing partial state.
func installAttentionHooks(
	root *os.Root,
	rel string,
	agent agenthook.Agent,
	migrate agentHookConfigMigration,
	registrations ...attentionHookRegistration,
) (bool, error) {
	original, err := root.ReadFile(rel)
	exists := true
	if errors.Is(err, os.ErrNotExist) {
		exists = false
		original = nil
	} else if err != nil {
		return false, err
	}

	staging, err := os.CreateTemp("", "kata-agent-hooks-*.json")
	if err != nil {
		return false, err
	}
	stagingPath := staging.Name()
	defer func() { _ = os.Remove(stagingPath) }()
	if _, err := staging.Write(original); err != nil {
		_ = staging.Close()
		return false, err
	}
	if err := staging.Close(); err != nil {
		return false, err
	}

	if migrate != nil {
		settings, err := decodeAgentHookJSON(original, !exists)
		if err != nil {
			return false, err
		}
		changed, err := migrate(settings)
		if err != nil {
			return false, err
		}
		if changed {
			encoded, err := encodeClaudeSettings(settings)
			if err != nil {
				return false, err
			}
			if err := os.WriteFile(stagingPath, encoded, 0o600); err != nil {
				return false, err
			}
		}
	}

	for _, registration := range registrations {
		if _, err := installAgentHookConfig(
			agent,
			attentionHookInstallOptions(stagingPath, registration.mode, registration.hook),
		); err != nil {
			return false, err
		}
	}
	final, err := os.ReadFile(stagingPath) //nolint:gosec // private staging file created above
	if err != nil {
		return false, err
	}
	if exists && bytes.Equal(original, final) {
		return false, nil
	}
	if err := writeAgentHookConfig(root, rel, final, original, exists); err != nil {
		return false, err
	}
	return true, nil
}

func attentionHookInstallOptions(
	configPath, mode string,
	hook agenthook.Hook,
) agenthook.InstallOptions {
	source := attentionHookSource + mode
	return agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", mode, "--source", source},
		Marker:     "--source " + source,
		Hooks:      []agenthook.Hook{hook},
	}
}

func writeAgentHookConfig(
	root *os.Root,
	rel string,
	data, original []byte,
	exists bool,
) error {
	if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
		return err
	}
	if exists {
		current, err := root.ReadFile(rel)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, original) {
			return fmt.Errorf("%s changed during hook installation", filepath.Join(root.Name(), rel))
		}
		return atomicReplaceSettings(root, rel, data)
	}
	return atomicCreateSettings(root, rel, data)
}

// validateManagedHookJSON retains kata's existing refusal to treat null JSON
// objects or managed event arrays as empty configuration. Kit owns all other
// parsing and structural validation in the staging file.
func validateManagedHookJSON(configPath string, events ...agenthook.Event) error {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	root, err := decodeAgentHookJSON(data, false)
	if err != nil {
		return err
	}
	rawHooks, exists := root["hooks"]
	if !exists {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return errors.New("hook config hooks must be an object")
	}
	for _, event := range events {
		if raw, exists := hooks[string(event)]; exists && raw == nil {
			return fmt.Errorf("hook config event %s must be an array", event)
		}
	}
	return nil
}

func decodeAgentHookJSON(data []byte, emptyOK bool) (map[string]any, error) {
	if emptyOK && len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("hook config root must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

// removeExactAgentHook removes only a byte-shape-equivalent shipped handler.
// A group that carries no other metadata is pruned when its final handler is
// removed; groups with unrelated fields remain intact.
func removeExactAgentHook(
	settings map[string]any,
	event agenthook.Event,
	matcher string,
	exactHandler map[string]any,
) (bool, error) {
	rawHooks, exists := settings["hooks"]
	if !exists {
		return false, nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return false, errors.New("hooks has an unexpected shape")
	}
	rawGroups, exists := hooks[string(event)]
	if !exists {
		return false, nil
	}
	groups, ok := rawGroups.([]any)
	if !ok {
		return false, fmt.Errorf("hooks.%s has an unexpected shape", event)
	}

	changed := false
	keptGroups := make([]any, 0, len(groups))
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok || group["matcher"] != matcher {
			keptGroups = append(keptGroups, rawGroup)
			continue
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			return false, fmt.Errorf("hooks.%s group hooks has an unexpected shape", event)
		}
		groupChanged := false
		keptHandlers := make([]any, 0, len(handlers))
		for _, handler := range handlers {
			if reflect.DeepEqual(handler, exactHandler) {
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
		delete(hooks, string(event))
	} else {
		hooks[string(event)] = keptGroups
	}
	return true, nil
}

// refuseSymlinkComponents rejects any existing symlink among the given
// root-relative paths. Paths that do not exist yet are created as real entries.
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

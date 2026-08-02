package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/agenthook"
)

func installAttentionHook(
	agent agenthook.Agent,
	configPath, mode string,
	hook agenthook.Hook,
) (bool, error) {
	result, err := agenthook.Install(agent, attentionHookInstallOptions(configPath, mode, hook))
	return result.Changed, err
}

func planAttentionHook(
	agent agenthook.Agent,
	configPath, mode string,
	hook agenthook.Hook,
) error {
	_, err := agenthook.PlanInstall(agent, attentionHookInstallOptions(configPath, mode, hook))
	return err
}

func attentionHookInstallOptions(
	configPath, mode string,
	hook agenthook.Hook,
) agenthook.InstallOptions {
	return agenthook.InstallOptions{
		ConfigPath: configPath,
		Executable: "kata",
		Arguments:  []string{"attention-hook", mode},
		Marker:     "kata attention-hook " + mode,
		Hooks:      []agenthook.Hook{hook},
	}
}

// validateManagedHookJSON retains kata's existing refusal to treat null JSON
// objects or managed event arrays as empty configuration. Kit owns all other
// parsing and structural validation during PlanInstall.
func validateManagedHookJSON(configPath string, events ...agenthook.Event) error {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return err
	}
	if root == nil {
		return errors.New("hook config root must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
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

// refuseSymlinkComponents rejects any existing symlink among the given
// root-relative paths. Paths that do not exist yet are created by kit as real
// entries.
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

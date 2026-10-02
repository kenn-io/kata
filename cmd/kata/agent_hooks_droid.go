package main

import "path/filepath"

func planDroidAgentHooks(opts nativeAgentHookOptions, remove bool, path string) (nativeAgentHookPlan, error) {
	data, exists, err := readNativeAgentHookFile(path)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	var preimages []nativeAgentHookChange
	wrapped := filepath.Base(path) == "settings.json"
	if opts.ConfigPath == "" && !exists {
		// Droid reads inline settings hooks only in the absence of hooks.json.
		// The absent primary file participates in the publisher's snapshot check.
		primary := path
		fallback := filepath.Join(filepath.Dir(path), "settings.json")
		fallbackData, fallbackExists, err := readNativeAgentHookFile(fallback)
		if err != nil {
			return nativeAgentHookPlan{}, err
		}
		if fallbackExists {
			path, data, exists, wrapped = fallback, fallbackData, true, true
			preimages = append(preimages, nativeAgentHookChange{Path: primary, Remove: true})
		} else {
			preimages = append(preimages, nativeAgentHookChange{Path: fallback, Remove: true})
		}
	}
	plan, content, err := planExtraJSONHooks(opts, remove, path, data, exists, wrapped)
	if err != nil {
		return nativeAgentHookPlan{}, err
	}
	plan.Changes = append(preimages, nativeAgentHookChange{Path: path, Original: data, OriginalExists: exists, Content: content, Remove: remove && !exists})
	return plan, nil
}

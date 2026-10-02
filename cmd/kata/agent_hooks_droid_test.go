package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDroidUsesSettingsFallbackWithoutShadowing(t *testing.T) {
	opts := extraOptions(t, "droid")
	path := filepath.Join(opts.Home, ".factory", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	before := []byte(`{"theme":"dark","hooks":{"SessionStart":[{"matcher":"resume","extra":true,"hooks":[{"type":"command","command":"echo foreign","custom":{"keep":1}}]}]},"future":12345678901234567890}`)
	require.NoError(t, os.WriteFile(path, before, 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.Equal(t, path, plan.Path)
	require.Len(t, plan.Changes, 2, "absence of hooks.json must remain a publication preimage")
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(opts.Home, ".factory", "hooks.json"))
	require.True(t, os.IsNotExist(err))
	data, err := os.ReadFile(path) //nolint:gosec // G304: generated configuration fixture stays inside the isolated test home.
	require.NoError(t, err)
	require.True(t, bytes.Contains(data, []byte(`{"type":"command","command":"echo foreign","custom":{"keep":1}}`)), "foreign handler bytes changed")
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	require.Equal(t, "dark", root["theme"])
	// An agent creating hooks.json after planning changes the fallback decision.
	repair, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(opts.Home, ".factory", "hooks.json"), []byte(`{}`), 0600))
	_, err = publishNativeAgentHookPlan(repair)
	require.ErrorContains(t, err, "changed after planning")
}

func TestExtraJSONOwnershipPreservesForeignHandlers(t *testing.T) {
	opts := extraOptions(t, "droid")
	opts.ConfigPath = filepath.Join(opts.Home, "hooks.json")
	foreign := []byte(`{"SessionStart":[{"hooks":[{"command":"kata agent-hooks contract droid --source authored.txt"},{"command":"kata agent-hooks contract droid","commandWindows":"echo foreign"},{"command":"kata agent-hooks contract droid && echo custom"},{"command":"kata agent-hooks contract droid","args":["--source","authored.txt"]}]}],"Stop":[{"hooks":[{"command":"echo stop"}]}]}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, foreign, 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.False(t, plan.CurrentContract)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	opts.Contract = true
	opts.Attention = true
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	require.JSONEq(t, string(foreign), string(data))
}

func TestExtraJSONRejectsMalformedAndSymlinkedFiles(t *testing.T) {
	for _, input := range []string{`null`, `{"SessionStart":null}`, `{"SessionStart":{}}`, `{"SessionStart":[{"hooks":{}}]}`, `{"SessionStart":[],"SessionStart":[]}`, `{"SessionStart":[1]}`} {
		opts := extraOptions(t, "droid")
		opts.ConfigPath = filepath.Join(opts.Home, "hooks.json")
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(input), 0600))
		_, err := planExtraAgentHooks(opts, false)
		require.Error(t, err)
		data, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, input, string(data))
	}
	opts := extraOptions(t, "droid")
	opts.ConfigPath = filepath.Join(opts.Home, "linked.json")
	target := filepath.Join(opts.Home, "operator.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0600))
	if err := os.Symlink(target, opts.ConfigPath); err != nil {
		t.Skip(err)
	}
	_, err := planExtraAgentHooks(opts, false)
	require.ErrorContains(t, err, "symlink")
}

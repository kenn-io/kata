package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMuseMalformedOwnedTypedFieldsRequireRepair(t *testing.T) {
	for _, field := range []string{
		`"timeout":"10"`, `"timeout":-1`, `"timeout":1.5`,
		`"statusMessage":false`, `"async":"yes"`,
		`"onFailure":"echo fallback"`,
		`"onFailure":{"type":"command","command":"echo fallback","timeout":"10"}`,
		`"outputCapabilities":"skills.v1"`, `"outputCapabilities":[17]`,
	} {
		t.Run(field, func(t *testing.T) {
			opts := extraOptions(t, "muse")
			opts.Attention = false
			opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
			handler := `"command":"kata agent-hooks contract muse",` + field
			if !strings.HasPrefix(field, `"type":`) {
				handler = `"type":"command",` + handler
			}
			before := []byte(`{"schema_version":1,"hooks":{"SessionStart":[{"hooks":[{` + handler + `}]}]}}`)
			require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
			inspect := opts
			inspect.Contract = false
			status, err := planExtraAgentHooks(inspect, true)
			require.NoError(t, err)
			require.False(t, status.CurrentContract)
			require.False(t, status.Contract)
			require.Contains(t, status.Warnings, museTypedFieldWarning(opts.ConfigPath), "source-wide rejection must be actionable")
			changed, err := publishNativeAgentHookPlan(status)
			require.NoError(t, err)
			require.False(t, changed)
			plan, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.True(t, plan.Contract, "explicit reinstall must repair the owned handler")
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			status, err = planExtraAgentHooks(inspect, true)
			require.NoError(t, err)
			require.True(t, status.CurrentContract)
		})
	}
}

func TestMuseForeignMalformedHandlerPoisonsWholeSource(t *testing.T) {
	for _, field := range []string{
		`"timeout":"10"`, `"statusMessage":false`, `"async":"yes"`,
		`"command":17`, `"commandWindows":null`, `"command_windows":17`, `"type":17`,
		`"onFailure":{"type":"command","command":"echo fallback","async":"yes"}`,
		`"outputCapabilities":false`,
	} {
		t.Run(field, func(t *testing.T) {
			opts := extraOptions(t, "muse")
			opts.Attention = false
			opts.Executable = "kata"
			opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
			foreignFields := field
			if !strings.HasPrefix(field, `"type":`) {
				foreignFields = `"type":"command",` + foreignFields
			}
			if !strings.HasPrefix(field, `"command":`) {
				foreignFields = `"command":"echo operator",` + foreignFields
			}
			foreign := `{` + foreignFields + `}`
			before := []byte(`{"schema_version":1,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hooks contract muse","timeout":10}]}],"PreToolUse":[{"hooks":[` + foreign + `]}]}}`)
			require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
			inspect := opts
			inspect.Contract = false
			status, err := planExtraAgentHooks(inspect, true)
			require.NoError(t, err)
			require.False(t, status.CurrentContract, "a malformed foreign sibling rejects the whole source")
			plan, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.False(t, plan.Contract, "preserved malformed foreign handlers keep the source inactive")
			require.Contains(t, plan.Warnings, museTypedFieldWarning(opts.ConfigPath))
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			data, err := os.ReadFile(opts.ConfigPath)
			require.NoError(t, err)
			require.Equal(t, before, data, "foreign malformed handlers must remain byte-stable")
		})
	}
}

func TestMuseMalformedManagedSiblingDisablesAttention(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.Contract = false
	opts.ManagedAttention = true
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"schema_version":1,"managed_hooks_path":"managed.json"}`), 0600))
	managedPath := filepath.Join(opts.Home, "managed.json")
	foreign := `{"type":"command","command":"echo operator","timeout":"10"}`
	require.NoError(t, os.WriteFile(managedPath, []byte(`{"hooks":{"PreToolUse":[{"hooks":[`+foreign+`]}]}}`), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.False(t, plan.AttentionStart)
	require.False(t, plan.AttentionEnd)
	require.Contains(t, plan.Warnings, museTypedFieldWarning(managedPath))
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(managedPath) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	require.Contains(t, string(data), foreign)
}

func museTypedFieldWarning(path string) string {
	return "Muse " + path + " has malformed handler field types; the whole native source is inactive until those fields are repaired"
}

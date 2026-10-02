package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMuseRequiresExplicitManagedAttention(t *testing.T) {
	opts := extraOptions(t, "muse")
	_, err := planExtraAgentHooks(opts, false)
	require.ErrorContains(t, err, "--managed-attention")
	opts.Attention = false
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, plan.Contract)
	require.False(t, plan.AttentionStart)
	require.False(t, plan.AttentionEnd)
	require.NotEmpty(t, plan.Warnings)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(plan.Path)
	require.NoError(t, err)
	var root map[string]any
	require.NoError(t, json.Unmarshal(data, &root))
	require.Equal(t, float64(1), root["schema_version"])
	require.NotContains(t, root, "managed_hooks_path")
	for _, scope := range []string{"project", "user"} {
		opts := extraOptions(t, "muse")
		opts.ManagedAttention = true
		opts.Scope = scope
		if scope == "project" {
			_, err := planExtraAgentHooks(opts, false)
			require.ErrorContains(t, err, "user")
		}
	}
}

func TestMuseManagedAttentionPreservesOperatorPolicyAndBundle(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.ManagedAttention = true
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	before := `{"schema_version":1,"theme":"dark","managed_hooks_path":"operator-hooks.json","managed_hooks_env_vars":["CI_TOKEN","KATA_REF"],"hooks":{"Stop":[{"hooks":[{"command":"echo foreign ordinary"}]}]}}`
	managed := `{"hooks":{"SessionStart":[{"matcher":"resume","hooks":[{"command":"echo foreign managed","timeout":11}]}]},"future":true}`
	managedPath := filepath.Join(opts.Home, "operator-hooks.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(before), 0600))
	require.NoError(t, os.WriteFile(managedPath, []byte(managed), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	managedWarning := strings.Join(plan.Warnings, "\n")
	require.Contains(t, managedWarning, "all managed hooks")
	require.Contains(t, managedWarning, "KATA_AUTH_TOKEN")
	require.Len(t, plan.Changes, 2)
	require.True(t, plan.AttentionStart)
	require.True(t, plan.AttentionEnd)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	require.Equal(t, "operator-hooks.json", settings["managed_hooks_path"])
	require.Equal(t, "dark", settings["theme"])
	allow := settings["managed_hooks_env_vars"].([]any)
	for _, name := range []string{"CI_TOKEN", "KATA_REF", "KATA_SERVER", "KATA_DAEMON", "KATA_HOME", "KATA_AUTHOR", "KATA_TEAMMATE", "KATA_INBOX_USER", "KATA_AUTH_TOKEN", "KATA_SESSION_ID"} {
		require.Contains(t, allow, name)
	}
	require.NotContains(t, allow, "KATA_WORKSPACE", "native cwd must stay authoritative")
	opts.Attention = false
	opts.ManagedAttention = false
	plan, err = planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, plan.CurrentAttentionStart)
	require.True(t, plan.AttentionStart)
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.False(t, changed)
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	require.False(t, plan.Contract)
	require.True(t, plan.AttentionStart)
	require.True(t, plan.AttentionEnd)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	opts.Contract = false
	opts.Attention = true
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err = os.ReadFile(managedPath) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	require.JSONEq(t, managed, string(data))
	// Policy names are retained because prior operator grants cannot be attributed
	// to a particular installer without an additional persistent ownership format.
	data, err = os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &settings))
	require.Equal(t, "operator-hooks.json", settings["managed_hooks_path"])
}

func TestMuseRejectsUnsupportedManagedPoliciesBeforeWrites(t *testing.T) {
	for _, settings := range []string{`{"schema_version":2}`, `{"hooks":{}}`, `{"schema_version":1,"managed_hooks_path":2}`, `{"schema_version":1,"managed_hooks_env_vars":["CI_TOKEN","ci_token"]}`, `{"schema_version":1,"managed_hooks_env_vars":["ANTHROPIC_API_KEY"]}`} {
		opts := extraOptions(t, "muse")
		opts.ManagedAttention = true
		opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(settings), 0600))
		_, err := planExtraAgentHooks(opts, false)
		require.Error(t, err)
		data, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, settings, string(data))
	}
	optsCase := extraOptions(t, "muse")
	optsCase.ManagedAttention = true
	optsCase.ConfigPath = filepath.Join(optsCase.Home, "settings.json")
	require.NoError(t, os.WriteFile(optsCase.ConfigPath, []byte(`{"schema_version":1,"managed_hooks_env_vars":["kata_ref"]}`), 0600))
	_, caseErr := planExtraAgentHooks(optsCase, false)
	require.ErrorContains(t, caseErr, "case")
	opts := extraOptions(t, "muse")
	opts.ManagedAttention = true
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"schema_version":1,"managed_hooks_path":"operator.json"}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(opts.Home, "operator.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"name":"policy","command":"echo policy","timeout_ms":10000}]}]}}`), 0600))
	_, err := planExtraAgentHooks(opts, false)
	require.ErrorContains(t, err, "strict")
}

func TestMuseStatusAndUninstallPreserveExistingProviderAllowlist(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	before := []byte(`{"schema_version":1,"managed_hooks_env_vars":["ANTHROPIC_API_KEY"],"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hooks contract muse","timeout":10}]}]}}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))

	status, err := planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	require.True(t, status.CurrentContract)

	removal, err := planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(removal)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	require.Equal(t, []any{"ANTHROPIC_API_KEY"}, settings["managed_hooks_env_vars"])
	require.NotContains(t, settings["hooks"].(map[string]any), "SessionStart")
}

func TestMuseExistingManagedContractUsesSharedOwnership(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.Attention = false
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"schema_version":1,"managed_hooks_path":"operator.json"}`), 0600))
	managedPath := filepath.Join(opts.Home, "operator.json")
	require.NoError(t, os.WriteFile(managedPath, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hooks contract muse","timeout":10}]}]}}`), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.True(t, plan.CurrentContract)
	require.True(t, plan.Contract)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	contractCount := func() int {
		count := 0
		for _, path := range []string{opts.ConfigPath, managedPath} {
			data, err := os.ReadFile(path) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
			require.NoError(t, err)
			root, err := parseExtraJSON(data, true)
			require.NoError(t, err)
			hooks, ok := root["hooks"].(map[string]any)
			if !ok {
				continue
			}
			registrations, err := extraJSONRegistrations("muse", map[string]map[string]any{"": hooks})
			require.NoError(t, err)
			for _, registration := range registrations {
				if extraHookKind("muse", registration.handler) == contractHook {
					count++
				}
			}
		}
		return count
	}
	require.Equal(t, 1, contractCount(), "existing managed contract should not be duplicated in ordinary hooks")
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	require.True(t, plan.CurrentContract)
	require.False(t, plan.Contract)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.Equal(t, 0, contractCount())
}

func TestMuseReinstallReconcilesDuplicateOwnedContracts(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.Attention = false
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	contract := `{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hooks contract muse","timeout":10}]}]}`
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(`{"schema_version":1,"managed_hooks_path":"operator.json","hooks":`+contract+`}`), 0600))
	managedPath := filepath.Join(opts.Home, "operator.json")
	require.NoError(t, os.WriteFile(managedPath, []byte(`{"hooks":`+contract+`}`), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(managedPath) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	root, err := parseExtraJSON(data, true)
	require.NoError(t, err)
	registrations, err := extraJSONRegistrations("muse", map[string]map[string]any{"": root["hooks"].(map[string]any)})
	require.NoError(t, err)
	for _, registration := range registrations {
		require.NotEqual(t, contractHook, extraHookKind("muse", registration.handler))
	}
	reinstall, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	changed, err := publishNativeAgentHookPlan(reinstall)
	require.NoError(t, err)
	require.False(t, changed)
}

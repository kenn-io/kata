package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMuseWindowsAliasOwnershipPreservesForeignVariants(t *testing.T) {
	for _, variant := range []string{`"echo authored Windows"`, `17`, `null`} {
		opts := extraOptions(t, "muse")
		opts.Attention = false
		opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
		before := []byte(`{"schema_version":1,"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract muse","command_windows":` + variant + `,"timeout":10}]}]}}`)
		require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
		plan, err := planExtraAgentHooks(opts, true)
		require.NoError(t, err)
		require.False(t, plan.CurrentContract)
		changed, err := publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		require.False(t, changed)
		data, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, before, data)
	}
}

func TestExtraJSONStatusAndRepairHonorNativeEligibility(t *testing.T) {
	for _, tc := range []struct {
		target, event, group, handler string
		contract, start, end          bool
	}{
		{"droid", "SessionStart", `"matcher":"^never$",`, `"type":"command","command":"kata agent-hook contract droid","timeout":10`, true, false, false},
		{"droid", "SessionStart", "", `"type":"command","command":"kata agent-hook attention-native droid start","timeout":10,"enabled":false`, false, true, false},
		{"droid", "SessionStart", "", `"type":"command","commandWindows":"kata agent-hook contract droid","timeout":10`, true, false, false},
		{"grok", "SessionEnd", `"matcher":"^never$",`, `"type":"command","command":"kata agent-hook attention-native grok end","timeout":10`, false, false, true},
		{"muse", "SessionStart", "", `"type":"command","command":"kata agent-hook contract muse","timeout":10,"async":true`, true, false, false},
		{"muse", "SessionStart", "", `"type":"command","command":"kata agent-hook contract muse","timeout":10,"if":"condition"`, true, false, false},
		{"antigravity", "PreInvocation", "", `"type":"command","command":"kata agent-hook contract antigravity","timeout":10,"enabled":false`, true, false, false},
	} {
		t.Run(tc.target+tc.handler, func(t *testing.T) {
			opts := extraOptions(t, tc.target)
			opts.ConfigPath = filepath.Join(opts.Home, "native.json")
			section := `{"` + tc.event + `":[{` + tc.group + `"hooks":[{` + tc.handler + `}]}]}`
			if tc.target == "antigravity" {
				section = `{"operator":{"` + tc.event + `":[{` + tc.handler + `}]}}`
			} else if tc.target != "droid" {
				section = `{"hooks":` + section + `}`
			}
			if tc.target == "muse" {
				section = `{"schema_version":1,` + section[1:]
			}
			before := []byte(section)
			require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
			inspect := opts
			inspect.Contract = false
			inspect.Attention = false
			plan, err := planExtraAgentHooks(inspect, true)
			require.NoError(t, err)
			require.False(t, plan.CurrentContract)
			require.False(t, plan.CurrentAttentionStart)
			require.False(t, plan.CurrentAttentionEnd)
			require.False(t, plan.Contract)
			require.False(t, plan.AttentionStart)
			require.False(t, plan.AttentionEnd)
			changed, err := publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			require.False(t, changed)
			if tc.target == "muse" {
				opts.Attention = false
			}
			plan, err = planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			if tc.contract {
				require.True(t, plan.Contract)
			}
			if tc.start {
				require.True(t, plan.AttentionStart)
			}
			if tc.end {
				require.True(t, plan.AttentionEnd)
			}
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			verified, err := planExtraAgentHooks(inspect, true)
			require.NoError(t, err)
			if tc.contract {
				require.True(t, verified.CurrentContract)
			}
			if tc.start {
				require.True(t, verified.CurrentAttentionStart)
			}
			if tc.end {
				require.True(t, verified.CurrentAttentionEnd)
			}
		})
	}
}

func TestExtraZCodePreservesExplicitDisabledGlobalPolicy(t *testing.T) {
	opts := extraOptions(t, "zcode")
	opts.ConfigPath = filepath.Join(opts.Home, "config.json")
	before := []byte(`{"hooks":{"enabled":false,"events":{"SessionStart":[{"hooks":[{"type":"process","command":"kata","args":["agent-hook","contract","zcode"],"timeoutMs":10000}]}]}}}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
	inspect := opts
	inspect.Contract = false
	inspect.Attention = false
	status, err := planExtraAgentHooks(inspect, true)
	require.NoError(t, err)
	require.False(t, status.CurrentContract)
	require.False(t, status.Contract)
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.False(t, plan.Contract)
	require.False(t, plan.AttentionStart)
	require.NotEmpty(t, plan.Warnings)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(plan.Path)
	require.NoError(t, err)
	root, err := parseExtraJSON(data, true)
	require.NoError(t, err)
	require.Equal(t, false, root["hooks"].(map[string]any)["enabled"])
}

func TestExtraZCodePreservesUnsetDisabledPolicyWhenForeignHooksExist(t *testing.T) {
	opts := extraOptions(t, "zcode")
	opts.ConfigPath = filepath.Join(opts.Home, "config.json")
	before := []byte(`{"hooks":{"timeoutMs":30000,"events":{"Stop":[{"hooks":[{"type":"process","command":"foreign-stop"}]}]}}}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))

	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	require.False(t, plan.Contract)
	require.False(t, plan.AttentionStart)
	require.False(t, plan.AttentionEnd)
	require.Contains(t, plan.Warnings, "ZCode hooks.enabled is unset while other hooks are present; preserving the disabled policy. Set hooks.enabled=true to activate configured hooks")
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	root, err := parseExtraJSON(data, true)
	require.NoError(t, err)
	hooks := root["hooks"].(map[string]any)
	require.NotContains(t, hooks, "enabled")
	events := hooks["events"].(map[string]any)
	require.Equal(t, "foreign-stop", events["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"])

	removal, err := planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(removal)
	require.NoError(t, err)
	data, err = os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	root, err = parseExtraJSON(data, true)
	require.NoError(t, err)
	hooks = root["hooks"].(map[string]any)
	require.NotContains(t, hooks, "enabled")
	events = hooks["events"].(map[string]any)
	require.Equal(t, "foreign-stop", events["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"])
	require.NotContains(t, events, "SessionStart")
}

func TestExtraTOMLStatusAndReinstallHonorRestrictiveMatcher(t *testing.T) {
	for _, tc := range []struct{ target, event, mode string }{
		{"kimi-code", "UserPromptSubmit", "contract"},
		{"kimi-code", "SessionStart", "attention-native kimi-code start"},
		{"kimi", "SessionEnd", "attention-native kimi end"},
	} {
		opts := extraOptions(t, tc.target)
		opts.ConfigPath = filepath.Join(opts.Home, "config.toml")
		arguments := tc.mode
		if tc.mode == "contract" {
			arguments = "contract kimi-code"
		}
		before := []byte("[[hooks]]\nevent='" + tc.event + "'\nmatcher='^never$'\ncommand='kata agent-hook " + arguments + "'\ntimeout=10\n")
		require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
		inspect := opts
		inspect.Contract = false
		inspect.Attention = false
		status, err := planExtraAgentHooks(inspect, true)
		require.NoError(t, err)
		require.False(t, status.CurrentContract)
		require.False(t, status.CurrentAttentionStart)
		require.False(t, status.CurrentAttentionEnd)
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		blocks, err := parseExtraTOMLHooks(plan.Changes[0].Content)
		require.NoError(t, err)
		for _, block := range blocks {
			require.NotContains(t, block.fields, "matcher", "explicit reinstall left a restrictive owned matcher")
		}
	}
}

func TestAntigravityInstallAndRemovalPreserveEmptyAuthoredBlocks(t *testing.T) {
	opts := extraOptions(t, "antigravity")
	opts.Attention = false
	opts.ConfigPath = filepath.Join(opts.Home, "hooks.json")
	before := []byte(`{"operator":{},"disabled-operator":{"enabled":false}}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	root, err := parseExtraJSON(data, true)
	require.NoError(t, err)
	require.Contains(t, root, "operator")
	require.Contains(t, root, "disabled-operator")
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err = os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(data))
}

func TestMuseRepairsInactiveManagedContractsWithoutDuplication(t *testing.T) {
	for _, ordinaryOwned := range []bool{false, true} {
		opts := extraOptions(t, "muse")
		opts.Attention = false
		opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
		settings := `{"schema_version":1,"managed_hooks_path":"managed.json"}`
		if ordinaryOwned {
			settings = `{"schema_version":1,"managed_hooks_path":"managed.json","hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract muse","timeout":10,"if":"disabled"}]}]}}`
		}
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(settings), 0600))
		managedPath := filepath.Join(opts.Home, "managed.json")
		require.NoError(t, os.WriteFile(managedPath, []byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata agent-hook contract muse","timeout":10,"async":true}]}]}}`), 0600))
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		require.False(t, plan.CurrentContract)
		require.True(t, plan.Contract)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
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
					require.True(t, extraJSONRegistrationEligible("muse", registration, contractHook))
				}
			}
		}
		require.Equal(t, 1, count)
	}
}

func TestMuseManagedAttentionPreservesAuthoredNumberLiterals(t *testing.T) {
	opts := extraOptions(t, "muse")
	opts.ManagedAttention = true
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	before := []byte(`{"schema_version":1,"preferences":{"counter":9007199254740993,"scale":1.2300e+02},"fraction":0.0001000}`)
	require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	root, err := parseExtraJSON(data, true)
	require.NoError(t, err)
	preferences := root["preferences"].(map[string]any)
	require.Equal(t, "9007199254740993", preferences["counter"].(interface{ String() string }).String())
	require.Equal(t, "1.2300e+02", preferences["scale"].(interface{ String() string }).String())
	require.Equal(t, "0.0001000", root["fraction"].(interface{ String() string }).String())
	require.Contains(t, string(data), `"preferences":{"counter":9007199254740993,"scale":1.2300e+02}`)
}

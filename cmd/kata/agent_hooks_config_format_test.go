package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDroidSetupPreservesChangedContainerIndentation(t *testing.T) {
	opts := extraOptions(t, "droid")
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	before := `{
    "zeta": { "numbers": [1e400,  2] },
    "hooks": {
        "SessionStart": [
            { "hooks": [{"command":"echo foreign"}] }
        ]
    },
    "alpha": true
}
`
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(before), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath) //nolint:gosec // Isolated native config fixture.
	require.NoError(t, err)
	require.Contains(t, string(data), "{\n    \"zeta\": { \"numbers\": [1e400,  2] },\n    \"hooks\": {\n        \"SessionStart\": [\n            { \"hooks\": [{\"command\":\"echo foreign\"}] },\n")
	require.Contains(t, string(data), "\n        ]")
	require.Contains(t, string(data), "\n    },\n    \"alpha\": true\n}\n")
}

func TestOpenClawSetupPreservesConfigOrderAndAuthoredSubtrees(t *testing.T) {
	isolateAgentHookHomes(t)
	opts := openClawTestOptions(t)
	opts.ConfigPath = filepath.Join(opts.Home, "openclaw.json")
	before := `{
    "zeta": { "numbers": [1e400,  2] },
    "plugins": {
        "load": {
            "paths": [
                "authored-plugin"
            ]
        }
    },
    "alpha": true
}
`
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(before), 0600))
	plan, err := planOpenClawAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath) //nolint:gosec // Isolated native config fixture.
	require.NoError(t, err)
	output := string(data)
	require.Less(t, strings.Index(output, `"zeta"`), strings.Index(output, `"plugins"`))
	require.Less(t, strings.Index(output, `"plugins"`), strings.Index(output, `"alpha"`))
	require.Contains(t, output, "{\n    \"zeta\": { \"numbers\": [1e400,  2] },\n")
	require.Contains(t, output, "\"plugins\": {\n        \"load\": {\n            \"paths\": [\n                \"authored-plugin\",\n")
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

func TestExtraTOMLPreservesAuthoredBlocksAndComments(t *testing.T) {
	opts := extraOptions(t, "kimi-code")
	opts.ConfigPath = filepath.Join(opts.Home, "config.toml")
	before := "# operator preferences\nmodel = 'example-model'\n\n[[hooks]] # operator hook\nevent = 'Stop'\ncommand = '''echo foreign\n[[hooks]]\n'''\ntimeout = 7 # seconds\n\n[other]\nvalue = 'keep' # keep this too\n"
	require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(before), 0600))
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	require.Contains(t, string(data), before)
	var document map[string]any
	_, err = toml.Decode(string(data), &document)
	require.NoError(t, err)
	require.Len(t, document["hooks"], 4)
	opts.Attention = false
	plan, err = planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	changed, err := publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	require.False(t, changed)
	opts.Attention = true
	plan, err = planExtraAgentHooks(opts, true)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	data, err = os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, before, string(data))
}

func TestExtraTOMLRejectsComplexHookLayoutsBeforeWriting(t *testing.T) {
	for _, input := range []string{
		`hooks = [{ event = "SessionStart", command = "echo foreign" }]`,
		"[[hooks]]\nevent='Stop'\ncommand='echo foreign'\n[hooks.policy]\nenabled=true\n",
		"[[hooks]]\nevent='Stop'\ncommand='echo foreign'\ntimeout=601\n",
		"[[hooks]]\nevent='Stop'\ncommand='echo foreign'\nunknown=true\n",
	} {
		opts := extraOptions(t, "kimi-code")
		opts.ConfigPath = filepath.Join(opts.Home, "config.toml")
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(input), 0600))
		_, err := planExtraAgentHooks(opts, false)
		require.Error(t, err)
		data, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, input, string(data))
	}
}

func FuzzExtraTOMLForeignPreservation(f *testing.F) {
	f.Add("echo native", "operator note")
	f.Add("kata agent-hooks contract kimi-code --source local.txt", "[[hooks]]")
	f.Fuzz(func(t *testing.T, command, note string) {
		// TOML encodes arbitrary strings (including header-looking data) itself.
		var input struct {
			Model string           `toml:"model"`
			Hooks []map[string]any `toml:"hooks"`
		}
		input.Model = note
		input.Hooks = []map[string]any{{"event": "Stop", "command": command, "timeout": 10}}
		var buffer strings.Builder
		require.NoError(t, toml.NewEncoder(&buffer).Encode(input))
		opts := extraOptions(t, "kimi-code")
		opts.ConfigPath = filepath.Join(opts.Home, "config.toml")
		require.NoError(t, os.WriteFile(opts.ConfigPath, []byte(buffer.String()), 0600))
		if !utf8.ValidString(command) || !utf8.ValidString(note) || strings.TrimSpace(command) == "" {
			_, err := planExtraAgentHooks(opts, false)
			require.Error(t, err, "invalid UTF-8 and empty shell commands are not native hook data")
			data, err := os.ReadFile(opts.ConfigPath)
			require.NoError(t, err)
			require.Equal(t, buffer.String(), string(data))
			return
		}
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		plan, err = planExtraAgentHooks(opts, true)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		data, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, buffer.String(), string(data))
	})
}

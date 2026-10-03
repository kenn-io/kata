package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

// The fixtures describe the native consumers' response contracts, rather than
// using the provider renderer to derive expected output.
func TestExtraAgentContractNativeProtocols(t *testing.T) {
	for _, tc := range []struct{ target, input, want string }{
		{"droid", `{"hook_event_name":"SessionStart","session_id":"session-1","cwd":"/workspace/example","source":"startup"}`, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"contract text"}}`},
		{"muse", `{"hook_event_name":"SessionStart","session_id":"session-1","cwd":"/workspace/example","source":"startup"}`, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"contract text"}}`},
		{"zcode", `{"hookEventName":"SessionStart","sessionId":"session-1","cwd":"/workspace/example","source":"startup"}`, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"contract text"}}`},
		{"antigravity", `{"conversationId":"session-1","workspacePaths":["/workspace/example"],"invocationNum":0,"initialNumSteps":0}`, `{"injectSteps":[{"ephemeralMessage":"contract text"}]}`},
		{"antigravity", `{"conversationId":"session-1","workspacePaths":["/workspace/example"],"invocationNum":1,"initialNumSteps":2}`, `{}`},
		{"kimi-code", `{"hook_event_name":"UserPromptSubmit","session_id":"session-1","cwd":"/workspace/example","prompt":"hello"}`, "contract text\n"},
	} {
		t.Run(tc.target+tc.input, func(t *testing.T) {
			var output bytes.Buffer
			require.NoError(t, writeExtraAgentContract(tc.target, strings.NewReader(tc.input), &output, "contract text"))
			if tc.target == "kimi-code" {
				require.Equal(t, tc.want, output.String())
			} else {
				require.JSONEq(t, tc.want, output.String())
			}
		})
	}
}

func TestExtraAgentContractRejectsInvalidInputBeforeOutput(t *testing.T) {
	for _, tc := range []struct{ target, input string }{
		{"droid", `{}`}, {"droid", `null`}, {"droid", `{"hook_event_name":"Stop","session_id":"s","cwd":"/workspace/example"}`},
		{"zcode", `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/workspace/example"}`},
		{"antigravity", `{"conversationId":"s","workspacePaths":[],"invocationNum":-1}`},
		{"antigravity", `{"conversationId":"s","workspacePaths":[],"invocationNum":0.5}`},
		{"antigravity", `{"conversationId":"s","workspacePaths":[]}`},
		{"kimi-code", `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/workspace/example"}`},
		{"kimi", `{"hook_event_name":"UserPromptSubmit","session_id":"s"}`}, {"grok", `{"hookEventName":"SessionStart","sessionId":"s"}`},
		{"muse", `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/workspace/example"} {}`},
	} {
		t.Run(tc.target+tc.input, func(t *testing.T) {
			var output bytes.Buffer
			require.Error(t, writeExtraAgentContract(tc.target, strings.NewReader(tc.input), &output, "text"))
			require.Empty(t, output.String())
		})
	}
}

func extraOptions(t *testing.T, target string) nativeAgentHookOptions {
	t.Helper()
	return nativeAgentHookOptions{Agent: target, Scope: "user", Home: t.TempDir(), Dir: t.TempDir(), Executable: "/opt/example/kata", Contract: true, Attention: true}
}

func TestExtraAgentPlansNativeEventsAndSelection(t *testing.T) {
	for _, tc := range []struct {
		target               string
		contract, start, end bool
		events               []string
	}{
		{"droid", true, true, true, []string{"SessionStart", "SessionEnd"}},
		{"antigravity", true, false, false, []string{"PreInvocation"}},
		{"zcode", true, true, false, []string{"SessionStart"}},
		{"kimi-code", true, true, true, []string{"UserPromptSubmit", "SessionStart", "SessionEnd"}},
		{"kimi", false, true, true, []string{"SessionStart", "SessionEnd"}},
		{"grok", false, true, true, []string{"SessionStart", "SessionEnd"}},
	} {
		t.Run(tc.target, func(t *testing.T) {
			opts := extraOptions(t, tc.target)
			plan, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.Equal(t, tc.contract, plan.Contract)
			require.Equal(t, tc.start, plan.AttentionStart)
			require.Equal(t, tc.end, plan.AttentionEnd)
			require.False(t, plan.CurrentContract)
			_, err = os.Stat(plan.Path)
			require.True(t, os.IsNotExist(err), "planning wrote a config")
			_, err = publishNativeAgentHookPlan(plan)
			require.NoError(t, err)
			var doc map[string]any
			data, err := os.ReadFile(plan.Path)
			require.NoError(t, err)
			if strings.HasSuffix(plan.Path, ".toml") {
				_, err = toml.Decode(string(data), &doc)
			} else {
				err = json.Unmarshal(data, &doc)
			}
			require.NoError(t, err)
			// Real parsed event sets catch accidental Stop/idle substitutions.
			require.ElementsMatch(t, tc.events, extraTestEvents(doc, tc.target))
			opts.Attention = false
			if !tc.contract {
				opts.Attention = true
			}
			reinstalled, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			require.Equal(t, tc.start, reinstalled.AttentionStart)
			require.Equal(t, tc.end, reinstalled.AttentionEnd)
			changed, err := publishNativeAgentHookPlan(reinstalled)
			require.NoError(t, err)
			require.False(t, changed, "no-op reinstall changed bytes")
			opts.Contract = true
			opts.Attention = false
			removed, err := planExtraAgentHooks(opts, true)
			require.NoError(t, err)
			require.False(t, removed.Contract)
			require.Equal(t, tc.start, removed.AttentionStart)
			require.Equal(t, tc.end, removed.AttentionEnd)
			_, err = publishNativeAgentHookPlan(removed)
			require.NoError(t, err)
			opts.Contract = false
			opts.Attention = true
			removed, err = planExtraAgentHooks(opts, true)
			require.NoError(t, err)
			require.False(t, removed.AttentionStart)
			require.False(t, removed.AttentionEnd)
		})
	}
}

func extraTestEvents(doc map[string]any, target string) []string {
	var events []string
	if target == "kimi-code" || target == "kimi" {
		for _, raw := range doc["hooks"].([]map[string]any) {
			events = append(events, raw["event"].(string))
		}
		return events
	}
	section := doc
	if target == "antigravity" {
		for _, block := range doc {
			for event := range block.(map[string]any) {
				if event != "enabled" {
					events = append(events, event)
				}
			}
		}
		return events
	}
	if target != "droid" {
		section = doc["hooks"].(map[string]any)
	}
	if target == "zcode" {
		section = section["events"].(map[string]any)
	}
	for event := range section {
		events = append(events, event)
	}
	return events
}

func TestExtraAgentRejectsUnsupportedBeforeWrites(t *testing.T) {
	for _, target := range []string{"droid", "antigravity", "zcode", "kimi-code", "kimi", "muse", "grok"} {
		opts := extraOptions(t, target)
		opts.SourceSet = true
		opts.Source = "example.txt"
		_, err := planExtraAgentHooks(opts, false)
		require.Error(t, err)
		entries, err := os.ReadDir(opts.Home)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
	for _, target := range []string{"zcode", "kimi-code", "kimi"} {
		opts := extraOptions(t, target)
		opts.Scope = "project"
		_, err := planExtraAgentHooks(opts, false)
		require.ErrorContains(t, err, "project")
	}
	for _, target := range []string{"kimi", "grok"} {
		opts := extraOptions(t, target)
		opts.Attention = false
		_, err := planExtraAgentHooks(opts, false)
		require.ErrorContains(t, err, "--attention")
	}
}

func TestExtraReinstallReplacesPluralCommands(t *testing.T) {
	opts := extraOptions(t, "droid")
	install := func() (string, []byte) {
		t.Helper()
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		data, err := os.ReadFile(plan.Path)
		require.NoError(t, err)
		return plan.Path, data
	}
	path, fresh := install()
	require.Contains(t, string(fresh), "agent-hook ")
	require.NoError(t, os.WriteFile(path, bytes.ReplaceAll(fresh, []byte("agent-hook "), []byte("agent-hooks ")), 0o600))
	_, reinstalled := install()
	require.JSONEq(t, string(fresh), string(reinstalled))
}

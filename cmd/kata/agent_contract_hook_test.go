package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"pgregory.net/rapid"
)

func assertCodexPrompt(t *testing.T, output, prompt string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(output), &envelope))
	require.Len(t, envelope, 1)
	var specific map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(envelope["hookSpecificOutput"], &specific))
	require.Len(t, specific, 2)
	var event, text string
	require.NoError(t, json.Unmarshal(specific["hookEventName"], &event))
	require.NoError(t, json.Unmarshal(specific["additionalContext"], &text))
	assert.Equal(t, "SessionStart", event)
	assert.Equal(t, prompt, text)
}

func TestAgentContractHookBare(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assertCodexPrompt(t, stdout, agentContractText)
}

func TestAgentContractHookCustomSource(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name, path, text string
		exists           bool
	}{
		{"relative", "prompt.txt", "custom \"quotes\"\nUnicode: 世界\n", true},
		{"spaces", "custom prompt.txt", "exact trailing newline\n", true},
		{"empty", "empty.txt", "", true},
		{"missing", "missing.txt", agentContractText, false},
		{"explicit marker filename", "./kata-agent-contract-hook", "a file named like the old marker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.exists {
				require.NoError(t, os.WriteFile(tc.path, []byte(tc.text), 0o600))
			}
			resetRunEEntered(t)
			resetFlags(t)
			out, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", tc.path)
			require.NoError(t, err)
			assert.Empty(t, stderr)
			assertCodexPrompt(t, out, tc.text)
		})
	}
	absolute := filepath.Join(t.TempDir(), "absolute prompt.txt")
	require.NoError(t, os.WriteFile(absolute, []byte("absolute"), 0o600))
	out, _, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", absolute)
	require.NoError(t, err)
	assertCodexPrompt(t, out, "absolute")
}

func TestAgentContractHooksLegacyMarkerKeepsBuiltInContract(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, content := range []string{"authored replacement", ""} {
		require.NoError(t, os.WriteFile("kata-agent-contract-hook", []byte(content), 0o600))
		out, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", "kata-agent-contract-hook")
		require.NoError(t, err, stderr)
		assertCodexPrompt(t, out, agentContractText)
		for _, agent := range []agenthook.Agent{agenthook.AgentClaude, agenthook.AgentCodex} {
			out, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(agent)), "agent-hooks", "contract", string(agent), "--source", "kata-agent-contract-hook")
			require.NoError(t, err, stderr)
			assertNativePrompt(t, agent, out, agentContractText)
		}
	}
}

func TestAgentContractHookFileErrors(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.txt")
	require.NoError(t, os.WriteFile(invalid, []byte{0xff}, 0o600))
	unreadable := filepath.Join(dir, "unreadable.txt")
	require.NoError(t, os.WriteFile(unreadable, []byte("private prompt contents"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o600) })
	for _, path := range []string{dir, invalid, unreadable} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if path == unreadable && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("mode-only unreadability requires an unprivileged POSIX process")
			}
			resetRunEEntered(t)
			resetFlags(t)
			out, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", path)
			require.Error(t, err)
			assert.Empty(t, out)
			assert.Contains(t, stderr, "source")
			assert.NotContains(t, stderr, "private prompt contents")
		})
	}
}

func TestAgentContractHookRejectsRelativeSourceEscapingCurrentDirectory(t *testing.T) {
	workspace := t.TempDir()
	private := filepath.Join(t.TempDir(), "private.txt")
	require.NoError(t, os.WriteFile(private, []byte("private prompt contents"), 0o600))
	link := filepath.Join(workspace, "prompt.txt")
	if err := os.Symlink(private, link); err != nil {
		t.Skipf("creating symlink unavailable: %v", err)
	}
	t.Chdir(workspace)

	resetRunEEntered(t)
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", "prompt.txt")
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "source")
	assert.NotContains(t, stderr, "private prompt contents")
}

func TestAgentContractHookRejectsRelativeParentSource(t *testing.T) {
	workspace := t.TempDir()
	private := filepath.Join(t.TempDir(), "private.txt")
	require.NoError(t, os.WriteFile(private, []byte("private prompt contents"), 0o600))
	relative, err := filepath.Rel(workspace, private)
	require.NoError(t, err)
	t.Chdir(workspace)

	resetRunEEntered(t)
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--source", relative)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "source")
	assert.NotContains(t, stderr, "private prompt contents")
}

func TestAgentContractHookUsage(t *testing.T) {
	for _, args := range [][]string{{"--source"}, {"--source="}, {"--unknown"}, {"extra"}} {
		t.Run(args[0], func(t *testing.T) {
			resetRunEEntered(t)
			resetFlags(t)
			out, _, err := executeRootCapture(t, context.Background(), append([]string{"agent-contract-hook"}, args...)...)
			require.Error(t, err)
			assert.Empty(t, out)
		})
	}
	out, _, err := executeRootCapture(t, context.Background(), "agent-contract-hook", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "--source")
	assert.Contains(t, out, "local")
}

func TestAgentHooksContractCustomSource(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("custom.txt", []byte("replacement"), 0o600))
	out, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(agenthook.AgentCodex)), "agent-hooks", "contract", "codex", "--source", "custom.txt")
	require.NoError(t, err, stderr)
	assertCodexPrompt(t, out, "replacement")
}

func TestAgentHooksContractSourceAllHarnesses(t *testing.T) {
	t.Chdir(t.TempDir())
	prompt := "custom \"prompt\"\nUnicode 世界\n"
	require.NoError(t, os.WriteFile("prompt.txt", []byte(prompt), 0o600))
	require.NoError(t, os.WriteFile("empty.txt", nil, 0o600))
	for _, agent := range []agenthook.Agent{agenthook.AgentClaude, agenthook.AgentCodex, agenthook.AgentCopilot, agenthook.AgentCursor, agenthook.AgentGemini, agenthook.AgentHermes, agenthook.AgentQwen} {
		t.Run(string(agent), func(t *testing.T) {
			out, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(agent)), "agent-hooks", "contract", string(agent), "--source", "prompt.txt")
			require.NoError(t, err, stderr)
			assertNativePrompt(t, agent, out, prompt)
			out, stderr, err = executeAgentHook(t, strings.NewReader(contractHookPayload(agent)), "agent-hooks", "contract", string(agent), "--source", "empty.txt")
			require.NoError(t, err, stderr)
			var response map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			// Native codecs may omit empty context; inspect every native context
			// field so escaped JSON cannot hide a default or prior prompt.
			if specific, ok := response["hookSpecificOutput"]; ok {
				var nested map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(specific, &nested))
				maps.Copy(response, nested)
			}
			for _, key := range []string{"additionalContext", "additional_context", "context"} {
				if value, ok := response[key]; ok {
					var text string
					require.NoError(t, json.Unmarshal(value, &text))
					assert.Empty(t, text)
				}
			}
		})
	}
}

func TestAgentContractHookPromptRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.txt")
	rapid.Check(t, func(rt *rapid.T) {
		prompt := rapid.String().Draw(rt, "prompt")
		if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
			rt.Fatal(err)
		}
		cmd := newAgentContractHookCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--source", path})
		if err := cmd.Execute(); err != nil {
			rt.Fatal(err)
		}
		var response struct {
			Output struct {
				Text string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out.Bytes(), &response); err != nil {
			rt.Fatal(err)
		}
		if response.Output.Text != prompt {
			rt.Fatalf("prompt did not round-trip: want %q, got %q", prompt, response.Output.Text)
		}
	})
}

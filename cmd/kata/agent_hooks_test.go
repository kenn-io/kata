package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

func contractHookPayload(agent agenthook.Agent) string {
	switch agent {
	case agenthook.AgentCursor:
		return `{"hook_event_name":"sessionStart","session_id":"example-session","source":"startup"}`
	case agenthook.AgentHermes:
		return `{"hook_event_name":"pre_llm_call","session_id":"example-session","extra":{"user_message":"example prompt","is_first_turn":true}}`
	case agenthook.AgentMuse:
		return `{"hook_event_name":"SessionStart","session_id":"example-session","source":"startup","cwd":"."}`
	default:
		return `{"hook_event_name":"SessionStart","session_id":"example-session","source":"startup"}`
	}
}

func assertNativeContract(t *testing.T, agent agenthook.Agent, output string) {
	t.Helper()
	assertNativePrompt(t, agent, output, agentContractText)
}

func assertNativePrompt(t *testing.T, agent agenthook.Agent, output, expected string) {
	t.Helper()
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	var text string
	switch agent {
	case agenthook.AgentCopilot:
		require.NoError(t, json.Unmarshal(response["additionalContext"], &text))
	case agenthook.AgentCursor:
		require.NoError(t, json.Unmarshal(response["additional_context"], &text))
	case agenthook.AgentHermes:
		require.NoError(t, json.Unmarshal(response["context"], &text))
	default:
		var specific struct {
			Event string `json:"hookEventName"`
			Text  string `json:"additionalContext"`
		}
		require.NoError(t, json.Unmarshal(response["hookSpecificOutput"], &specific))
		assert.Equal(t, "SessionStart", specific.Event)
		text = specific.Text
	}
	assert.Equal(t, expected, text)
}

func executeAgentHook(t *testing.T, input io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetFlags(t)
	return executeAgentHookRoot(t, newRootCmd(), input, args...)
}

func executeAgentHookRoot(t *testing.T, cmd *cobra.Command, input io.Reader, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetIn(input)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(t.Context())
	if err != nil {
		emitRootError(&diagnostic, cmd, args, err, runEEntered)
	}
	return out.String(), diagnostic.String(), err
}

func TestAgentHooksContractTerminal(t *testing.T) {
	resetFlags(t)
	root := newRootCmd()
	group, _, err := root.Find([]string{"agent-hook"})
	require.NoError(t, err)
	root.RemoveCommand(group)
	root.AddCommand(newAgentHooksCmdWithTerminalCheck(func(io.Reader) bool { return true }))
	stdout, stderr, err := executeAgentHookRoot(t, root, unreadableHookInput{}, "agent-hook", "contract", "claude")
	require.Error(t, err)
	assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "kata quickstart --format contract")
}

type failedHookInput struct{}

func (failedHookInput) Read([]byte) (int, error) { return 0, errors.New("input failed\nextra detail") }

func TestAgentHooksContractReadError(t *testing.T) {
	stdout, stderr, err := executeAgentHook(t, failedHookInput{}, "agent-hook", "contract", "cursor")
	require.Error(t, err)
	assert.Equal(t, ExitInternal, exitCodeForErr(err, runEEntered))
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "input failed")
	assert.Equal(t, 1, strings.Count(stderr, "\n"))
}

func TestAgentHooksContractCursorOnlySessionStart(t *testing.T) {
	stdout, stderr, err := executeAgentHook(t, strings.NewReader(`{"hook_event_name":"sessionEnd","session_id":"example-session","reason":"complete"}`), "agent-hook", "contract", "cursor")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.JSONEq(t, `{}`, stdout)
	for _, payload := range []string{`{"hook_event_name":"sessionStart"`, `{"hook_event_name":"unknown"}`} {
		stdout, stderr, err := executeAgentHook(t, strings.NewReader(payload), "agent-hook", "contract", "cursor")
		require.Error(t, err)
		assert.Equal(t, ExitInternal, exitCodeForErr(err, runEEntered))
		assert.Empty(t, stdout)
		assert.Equal(t, 1, strings.Count(stderr, "\n"))
	}
}

func TestAgentHooksContractHermesSessionStartIsNeutral(t *testing.T) {
	stdout, stderr, err := executeAgentHook(t, strings.NewReader(`{"hook_event_name":"on_session_start","session_id":"example-session","extra":{"is_first_turn":true}}`), "agent-hook", "contract", "hermes")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.JSONEq(t, `{}`, stdout)
}

func TestAgentHooksContractKitManagedProfilesNativeResponses(t *testing.T) {
	t.Chdir(writeKataWorkspace(t, t.TempDir()))
	for _, profile := range agenthook.Profiles() {
		if !agentHookUsesKit(string(profile.Agent)) {
			continue
		}
		if !slices.Contains(profile.SupportedEvents, agenthook.EventSessionStart) {
			continue
		}
		t.Run(string(profile.Agent), func(t *testing.T) {
			for _, outputFlag := range []string{"", "--agent", "--json"} {
				args := []string{"agent-hook", "contract", string(profile.Agent)}
				if outputFlag != "" {
					args = append(args, outputFlag)
				}
				plain, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(profile.Agent)), args...)
				require.NoError(t, err)
				assert.Empty(t, stderr)
				assertNativeContract(t, profile.Agent, plain)
				managed, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(profile.Agent)), append(args, "--source", "kata-agent-contract-hook")...)
				require.NoError(t, err)
				assert.Empty(t, stderr)
				assert.Equal(t, plain, managed, "repeated session IDs and ownership markers do not gate the contract")
			}
		})
	}
}

// writeKataWorkspace binds dir the way kata init does and returns it.
func writeKataWorkspace(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("version = 1\n\n[project]\nname = \"example-project\"\n"), 0o600))
	return dir
}

func assertContractOrNone(t *testing.T, agent agenthook.Agent, out, text string) {
	t.Helper()
	if text == "" {
		assertNoNativeContext(t, out)
		return
	}
	assertNativePrompt(t, agent, out, text)
}

func TestAgentHooksContractOnlyInKataWorkspaces(t *testing.T) {
	workspace := writeKataWorkspace(t, t.TempDir())
	nested := filepath.Join(workspace, "src", "pkg")
	require.NoError(t, os.MkdirAll(nested, 0o700))
	outside := t.TempDir()
	t.Chdir(outside)

	for _, tc := range []struct {
		agent   string
		payload func(dir string) string
		want    func(t *testing.T, out, text string)
	}{
		{"codex", func(dir string) string {
			return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"s","source":"startup","cwd":%q}`, dir)
		}, func(t *testing.T, out, text string) { assertContractOrNone(t, agenthook.AgentCodex, out, text) }},
		{"claude", func(dir string) string {
			return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"s","source":"startup","cwd":%q}`, dir)
		}, func(t *testing.T, out, text string) { assertContractOrNone(t, agenthook.AgentClaude, out, text) }},
		{"droid", func(dir string) string {
			return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"s","source":"startup","cwd":%q}`, dir)
		}, func(t *testing.T, out, text string) { assertContractOrNone(t, agenthook.AgentClaude, out, text) }},
		{"antigravity", func(dir string) string {
			return fmt.Sprintf(`{"conversationId":"s","workspacePaths":[%q],"invocationNum":0,"initialNumSteps":0}`, dir)
		}, func(t *testing.T, out, text string) {
			if text == "" {
				assert.JSONEq(t, `{}`, out)
				return
			}
			assert.JSONEq(t, fmt.Sprintf(`{"injectSteps":[{"ephemeralMessage":%q}]}`, text), out)
		}},
		{"kimi-code", func(dir string) string {
			return fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"hello","cwd":%q}`, dir)
		}, func(t *testing.T, out, text string) {
			if text == "" {
				assert.Empty(t, out)
				return
			}
			assert.Equal(t, text+"\n", out)
		}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			out, stderr, err := executeAgentHook(t, strings.NewReader(tc.payload(nested)), "agent-hook", "contract", tc.agent)
			require.NoError(t, err, stderr)
			tc.want(t, out, agentContractText)

			out, stderr, err = executeAgentHook(t, strings.NewReader(tc.payload(outside)), "agent-hook", "contract", tc.agent)
			require.NoError(t, err, stderr)
			tc.want(t, out, "")
		})
	}

	t.Run("payload without cwd uses the hook's directory", func(t *testing.T) {
		payload := `{"hook_event_name":"SessionStart","session_id":"s","source":"startup"}`
		out, stderr, err := executeAgentHook(t, strings.NewReader(payload), "agent-hook", "contract", "codex")
		require.NoError(t, err, stderr)
		assertNoNativeContext(t, out)

		t.Chdir(nested)
		out, stderr, err = executeAgentHook(t, strings.NewReader(payload), "agent-hook", "contract", "codex")
		require.NoError(t, err, stderr)
		assertNativePrompt(t, agenthook.AgentCodex, out, agentContractText)
	})
}

type unreadableHookInput struct{}

func (unreadableHookInput) Read([]byte) (int, error) { panic("stdin must not be read") }

func TestAgentHooksContractUsage(t *testing.T) {
	for name, args := range map[string][]string{
		"missing harness": {},
		"extra harness":   {"claude", "codex"},
		"unknown harness": {"unknown"},
		"attention only":  {"kimi"},
		"missing source":  {"claude", "--source"},
		"empty source":    {"claude", "--source="},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"agent-hook", "contract"}, args...)...)
			require.Error(t, err)
			assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			assert.Equal(t, 1, strings.Count(stderr, "\n"))
			if name == "unknown harness" {
				for _, profile := range agenthook.Profiles() {
					assert.Contains(t, stderr, string(profile.Agent))
				}
			}
		})
	}
}

func TestAgentHooksContractPayloadErrors(t *testing.T) {
	for name, payload := range map[string]string{
		"empty": "", "malformed": "{", "null": "null", "multiple": "{} {}",
		"missing event": `{"source":"startup"}`, "missing source": `{"hook_event_name":"SessionStart"}`,
		"oversized": strings.Repeat(" ", (16<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, err := executeAgentHook(t, strings.NewReader(payload), "agent-hook", "contract", "claude")
			require.Error(t, err)
			assert.Equal(t, ExitInternal, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			assert.Equal(t, 1, strings.Count(stderr, "\n"))
		})
	}
}

func TestAgentHooksContractHermesFirstTurn(t *testing.T) {
	for _, firstTurn := range []string{`true`, `false`, `null`, `"true"`, `0`, ``} {
		t.Run(fmt.Sprintf("flag_%s", firstTurn), func(t *testing.T) {
			flag := ""
			if firstTurn != "" {
				flag = `,"is_first_turn":` + firstTurn
			}
			payload := `{"hook_event_name":"pre_llm_call","session_id":"example-session","extra":{"user_message":"example prompt"` + flag + `}}`
			stdout, stderr, err := executeAgentHook(t, strings.NewReader(payload), "agent-hook", "contract", "hermes")
			require.NoError(t, err)
			assert.Empty(t, stderr)
			if firstTurn == "true" {
				assertNativeContract(t, agenthook.AgentHermes, stdout)
			} else {
				assert.JSONEq(t, `{}`, stdout)
			}
		})
	}
}

func TestAgentHooksHelp(t *testing.T) {
	stdout, stderr, err := executeAgentHook(t, unreadableHookInput{}, "--help")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, 1, strings.Count(stdout, "agent-hook"))
	assert.NotContains(t, stdout, "agent-hooks")
	assert.Contains(t, stdout, "agent-contract-hook")
	assert.NotContains(t, stdout, "attention-hook")
}

func TestAgentHooksCompletion(t *testing.T) {
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile("example-file.txt", nil, 0o600))
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"agent-h"}, []string{"agent-hook"}},
		{[]string{"agent-hook", "contract", ""}, []string{"claude", "codex", "copilot", "cursor", "gemini", "hermes", "qwen", "droid", "antigravity", "kimi-code", "muse", "zcode"}},
		{[]string{"agent-hook", "contract", "c"}, []string{"claude", "codex", "copilot", "cursor"}},
		{[]string{"agent-hook", "contract", "claude", ""}, nil},
		{[]string{"agent-hook", "attention", "start", ""}, nil},
		{[]string{"agent-hook", "attention", "end", ""}, nil},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			stdout, _, err := executeAgentHook(t, unreadableHookInput{}, append([]string{"__complete"}, tc.args...)...)
			require.NoError(t, err)
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			require.NotEmpty(t, lines)
			directive, err := strconv.Atoi(strings.TrimPrefix(lines[len(lines)-1], ":"))
			require.NoError(t, err)
			assert.Equal(t, int(cobra.ShellCompDirectiveNoFileComp), directive)
			var names []string
			for _, line := range lines[:len(lines)-1] {
				name, _, _ := strings.Cut(line, "\t")
				names = append(names, name)
			}
			assert.ElementsMatch(t, tc.want, names)
		})
	}
}

// The singular-only public contract deliberately rejects the old spelling.
func TestAgentHookPluralCommandRejected(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	for _, suffix := range [][]string{
		nil, {"--help"}, {"contract", "claude"}, {"attention", "start"},
		{"install", "codex"}, {"uninstall", "codex"}, {"status"},
		{"status", "--agent"}, {"status", "--json"},
	} {
		t.Run(strings.Join(suffix, "_"), func(t *testing.T) {
			stdout, stderr, err := executeAgentHook(t, strings.NewReader(contractHookPayload(agenthook.AgentClaude)), append([]string{"agent-hooks"}, suffix...)...)
			require.ErrorContains(t, err, `unknown command "agent-hooks"`)
			assert.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			assert.Contains(t, stderr, "unknown command")
			assert.Contains(t, stderr, "agent-hooks")
		})
	}
}

// Hooks written before the agent-hook rename stay Kata-owned, so reinstalling
// replaces them instead of leaving a failing plural command beside the new one.
func TestAgentHookReinstallReplacesPluralCommands(t *testing.T) {
	isolateAgentHookHomes(t)
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "settings.json")
	install := func() []byte {
		t.Helper()
		_, _, err := executeAgentHook(t, unreadableHookInput{}, "agent-hook", "install", "claude", "--config", path, "--executable", os.Args[0])
		require.NoError(t, err)
		data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temporary config.
		require.NoError(t, err)
		return data
	}
	fresh := install()
	plural := bytes.ReplaceAll(fresh, []byte("agent-hook "), []byte("agent-hooks "))
	// Claude on Windows stores each argument as a separate JSON string.
	plural = bytes.ReplaceAll(plural, []byte(`"agent-hook"`), []byte(`"agent-hooks"`))
	require.NotEqual(t, string(fresh), string(plural))
	require.NoError(t, os.WriteFile(path, plural, 0o600))
	assert.Equal(t, string(fresh), string(install()))
}

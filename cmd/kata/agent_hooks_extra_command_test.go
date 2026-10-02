package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
)

// This process fixture receives the exact shell command emitted by the codec.
// It uses the production contract renderer and native payload reader, so the
// probes catch argv quoting, event registration and stdin transport together.
func TestExtraAgentCommandFixture(_ *testing.T) {
	if os.Getenv("KATA_EXTRA_COMMAND_FIXTURE") != "1" {
		return
	}
	var argv []string
	for i, arg := range os.Args {
		if arg == "--" {
			argv = os.Args[i+1:]
			break
		}
	}
	if len(argv) < 3 || argv[0] != "agent-hooks" {
		fmt.Fprintln(os.Stderr, "unexpected native hook argv")
		os.Exit(2)
	}
	if len(argv) >= 2 && argv[len(argv)-2] == "--source" {
		marker := "kata-agent-contract-hook"
		if argv[1] == "attention-native" && len(argv) == 6 {
			marker = "kata-agent-hook-" + argv[3]
		}
		if argv[len(argv)-1] != marker {
			fmt.Fprintln(os.Stderr, "unexpected hook ownership marker")
			os.Exit(2)
		}
		argv = argv[:len(argv)-2]
	}
	if argv[1] == "contract" && len(argv) == 3 {
		if err := writeExtraAgentContract(argv[2], os.Stdin, os.Stdout, "fixture contract"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		os.Exit(0)
	}
	if argv[1] == "attention-native" && len(argv) == 4 && (argv[3] == "start" || argv[3] == "end") {
		session, cwd, err := readNativeAttentionPayload(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		data, _ := json.Marshal(map[string]string{"target": argv[2], "mode": argv[3], "session": session, "cwd": cwd})
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unexpected native hook argv")
	os.Exit(5)
}

func TestExtraAgentEmittedCommandsConsumeNativeFixtures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell transport probe; Windows platform argv is covered independently")
	}
	testExecutable, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	launcher := filepath.Join(dir, "kata 'literal $ name")
	command, err := agenthook.BuildCommand(testExecutable, "-test.run=^TestExtraAgentCommandFixture$", "--")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(launcher, []byte("#!/bin/sh\nexec "+command.POSIX+" \"$@\"\n"), 0700)) //nolint:gosec // G306: temporary launcher requires execute permission to probe the emitted native command.
	t.Setenv("KATA_EXTRA_COMMAND_FIXTURE", "1")
	for _, target := range []string{"droid", "antigravity", "zcode", "kimi-code", "kimi", "muse", "grok"} {
		t.Run(target, func(t *testing.T) {
			opts := extraOptions(t, target)
			opts.Executable = launcher
			if target == "muse" {
				opts.ManagedAttention = true
			}
			plan, err := planExtraAgentHooks(opts, false)
			require.NoError(t, err)
			var calls []struct {
				event   string
				handler map[string]any
			}
			for _, change := range plan.Changes {
				if change.Remove || len(change.Content) == 0 {
					continue
				}
				if target == "kimi-code" || target == "kimi" {
					blocks, err := parseExtraTOMLHooks(change.Content)
					require.NoError(t, err)
					for _, block := range blocks {
						calls = append(calls, struct {
							event   string
							handler map[string]any
						}{block.fields["event"].(string), block.fields})
					}
					continue
				}
				root, err := parseExtraJSON(change.Content, true)
				require.NoError(t, err)
				sections := map[string]map[string]any{}
				if target == "antigravity" {
					for block, raw := range root {
						sections[block] = raw.(map[string]any)
					}
				} else {
					section := root
					if target != "droid" {
						section = root["hooks"].(map[string]any)
					}
					if target == "zcode" {
						section = section["events"].(map[string]any)
					}
					sections[""] = section
				}
				registrations, err := extraJSONRegistrations(target, sections)
				require.NoError(t, err)
				for _, registration := range registrations {
					calls = append(calls, struct {
						event   string
						handler map[string]any
					}{registration.event, registration.handler})
				}
			}
			for _, call := range calls {
				var payload string
				switch target {
				case "antigravity":
					payload = `{"conversationId":"native-session","workspacePaths":["/workspace/example"],"invocationNum":0,"initialNumSteps":0}`
				case "zcode", "grok":
					payload = fmt.Sprintf(`{"hookEventName":%q,"sessionId":"native-session","cwd":"/workspace/example","source":"startup"}`, call.event)
				default:
					payload = fmt.Sprintf(`{"hook_event_name":%q,"session_id":"native-session","cwd":"/workspace/example","source":"startup","prompt":"hello"}`, call.event)
				}
				var process *exec.Cmd
				if call.handler["type"] == "process" {
					raw := call.handler["args"].([]any)
					args := make([]string, len(raw))
					for i, arg := range raw {
						args[i] = arg.(string)
					}
					process = exec.Command(call.handler["command"].(string), args...) //nolint:gosec // G204: execute only the trusted provider-generated fixture command and argv.
				} else {
					process = exec.Command("sh", "-c", call.handler["command"].(string)) //nolint:gosec // G204: probe the exact trusted provider-generated shell command for this fixture.
				}
				process.Stdin = strings.NewReader(payload)
				output, err := process.CombinedOutput()
				require.NoError(t, err, string(output))
				kind := extraHookKind(target, call.handler)
				require.NotEmpty(t, kind, "installed hook ownership is lost")
				if kind == contractHook {
					switch target {
					case "kimi-code":
						require.Equal(t, "fixture contract\n", string(output))
					case "antigravity":
						require.JSONEq(t, `{"injectSteps":[{"ephemeralMessage":"fixture contract"}]}`, string(output))
					default:
						require.JSONEq(t, `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"fixture contract"}}`, string(output))
					}
				} else {
					require.JSONEq(t, fmt.Sprintf(`{"target":%q,"mode":%q,"session":"native-session","cwd":"/workspace/example"}`, target, string(kind)), string(output))
				}
			}
		})
	}
}

func TestExtraOwnershipHandlesLiteralExecutablePathsAcrossPlatforms(t *testing.T) {
	for _, path := range []string{"/opt/example/kata", `/opt/example path'$(printf owned)/kata`, `/opt/example=value/kata`, `C:\Program Files\Example\kata.exe`} {
		for _, kind := range []agentHookKind{contractHook, attentionStartHook, attentionEndHook} {
			commands, err := agenthook.BuildCommand(path, extraHookArguments("droid", kind)...)
			require.NoError(t, err)
			for _, handler := range []map[string]any{{"command": commands.Native, "commandWindows": commands.Windows}, {"bash": commands.POSIX, "powershell": commands.PowerShell}} {
				require.Equal(t, kind, extraHookKind("droid", handler), path)
			}
			argv, ok := literalAgentHookWords(commands.Windows, false, true)
			require.True(t, ok, path)
			require.Equal(t, path, argv[0])
			require.Equal(t, kind, extraHookArgvKind("droid", argv))
			museCommands, err := agenthook.BuildCommand(path, extraHookArguments("muse", kind)...)
			require.NoError(t, err)
			require.Equal(t, kind, extraHookKind("muse", map[string]any{"bash": museCommands.POSIX, "command_windows": museCommands.Windows}))
		}
	}
}

func FuzzExtraJSONForeignPreservation(f *testing.F) {
	for i := range byte(5) {
		f.Add(i, "echo foreign", "operator note")
	}
	f.Fuzz(func(t *testing.T, targetIndex byte, command, note string) {
		target := []string{"droid", "antigravity", "zcode", "muse", "grok"}[int(targetIndex)%5]
		opts := extraOptions(t, target)
		opts.ConfigPath = filepath.Join(opts.Home, "native.json")
		if target == "muse" {
			opts.Attention = false
		}
		if !utf8.ValidString(command) || !utf8.ValidString(note) {
			invalid := note
			if utf8.ValidString(note) {
				invalid = command
			}
			before := append([]byte(`{"foreign":"`), []byte(invalid)...)
			before = append(before, []byte(`"}`)...)
			require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
			_, err := planExtraAgentHooks(opts, false)
			require.Error(t, err, "native JSON requires valid UTF-8")
			after, err := os.ReadFile(opts.ConfigPath)
			require.NoError(t, err)
			require.Equal(t, before, after)
			return
		}
		// Authored args keep generated commands foreign. Muse instead pairs an
		// exact Kata command with an authored Windows variant to cover ownership
		// across every native platform command field.
		handler := map[string]any{"command": command, "args": []any{"authored"}, "metadata": note}
		var root map[string]any
		if target == "antigravity" {
			root = map[string]any{"operator": map[string]any{"Stop": []any{handler}}}
		} else {
			event := map[string]any{"Stop": []any{map[string]any{"hooks": []any{handler}, "matcher": "operator"}}}
			root = event
			if target != "droid" {
				root = map[string]any{"hooks": event, "preferences": note}
			}
			if target == "zcode" {
				handler["type"] = "process"
				root["hooks"] = map[string]any{"enabled": true, "events": event}
			}
			if target == "muse" {
				root["schema_version"] = 1
				delete(handler, "args")
				handler["command"] = "kata agent-hooks contract muse"
				handler["command_windows"] = "echo " + command
			}
		}
		before, err := json.Marshal(root)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(opts.ConfigPath, before, 0600))
		plan, err := planExtraAgentHooks(opts, false)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		plan, err = planExtraAgentHooks(opts, true)
		require.NoError(t, err)
		_, err = publishNativeAgentHookPlan(plan)
		require.NoError(t, err)
		after, err := os.ReadFile(opts.ConfigPath)
		require.NoError(t, err)
		require.JSONEq(t, string(before), string(after))
	})
}

func TestExtraContractRejectsOversizeAndPropagatesOutputFailure(t *testing.T) {
	var output bytes.Buffer
	require.ErrorContains(t, writeExtraAgentContract("droid", strings.NewReader(strings.Repeat(" ", 1024*1024+1)), &output, "text"), "too large")
	require.Empty(t, output.String())
	payload := `{"hook_event_name":"SessionStart","session_id":"s","cwd":"/workspace/example"}`
	require.ErrorIs(t, writeExtraAgentContract("droid", strings.NewReader(payload), extraFailWriter{}, "text"), io.ErrClosedPipe)
}

type extraFailWriter struct{}

func (extraFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

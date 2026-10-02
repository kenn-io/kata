package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kit/agenthook"
)

func TestMuseManagedRemoteCommandFixture(_ *testing.T) {
	if os.Getenv("KATA_EXTRA_MANAGED_REMOTE_FIXTURE") != "1" {
		return
	}
	var argv []string
	for i, arg := range os.Args {
		if arg == "--" {
			argv = os.Args[i+1:]
			break
		}
	}
	if strings.Join(argv, " ") != "agent-hooks attention-native muse start" {
		fmt.Fprintln(os.Stderr, "unexpected managed hook argv")
		os.Exit(2)
	}
	_, cwd, err := readNativeAttentionPayload(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	selection, err := client.InspectSelection(context.Background(), cwd, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	data, _ := json.Marshal(map[string]any{"base_url": selection.Resolved.BaseURL, "allow_insecure": selection.Resolved.AllowInsecure})
	_, _ = os.Stdout.Write(data)
	os.Exit(0)
}

func TestMuseManagedHandlerForwardsOperatorRemoteTransportPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX emitted-command process probe")
	}
	opts := extraOptions(t, "muse")
	opts.Contract = false
	opts.ManagedAttention = true
	opts.ConfigPath = filepath.Join(opts.Home, "settings.json")
	testExecutable, err := os.Executable()
	require.NoError(t, err)
	launcherCommand, err := agenthook.BuildCommand(testExecutable, "-test.run=^TestMuseManagedRemoteCommandFixture$", "--")
	require.NoError(t, err)
	opts.Executable = filepath.Join(t.TempDir(), "kata")
	require.NoError(t, os.WriteFile(opts.Executable, []byte("#!/bin/sh\nexec "+launcherCommand.POSIX+" \"$@\"\n"), 0700)) //nolint:gosec // G306: executable permission is required for the native launcher fixture.
	plan, err := planExtraAgentHooks(opts, false)
	require.NoError(t, err)
	_, err = publishNativeAgentHookPlan(plan)
	require.NoError(t, err)
	settingsData, err := os.ReadFile(opts.ConfigPath)
	require.NoError(t, err)
	settings, err := parseExtraJSON(settingsData, true)
	require.NoError(t, err)
	managedPath := filepath.Join(filepath.Dir(opts.ConfigPath), settings["managed_hooks_path"].(string))
	managedData, err := os.ReadFile(managedPath) //nolint:gosec // G304: fixture file is created by the test under its temporary home or SDK setup.
	require.NoError(t, err)
	managed, err := parseExtraJSON(managedData, true)
	require.NoError(t, err)
	registrations, err := extraJSONRegistrations("muse", map[string]map[string]any{"": managed["hooks"].(map[string]any)})
	require.NoError(t, err)
	var emitted string
	for _, registration := range registrations {
		if registration.event == "SessionStart" {
			emitted = registration.handler["command"].(string)
			break
		}
	}
	require.NotEmpty(t, emitted)
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			launching := map[string]string{"KATA_SERVER": "http://daemon.example:7777", "KATA_HOME": opts.Home, "KATA_TRUST_PRIVATE_NETWORK": "1"}
			if approved {
				launching["KATA_ALLOW_INSECURE"] = "1"
			}
			process := exec.Command("sh", "-c", emitted) //nolint:gosec // G204: controlled native fixture command or current test executable, with fixed test arguments.
			// Model Muse's documented managed allowlist from generated settings,
			// rather than inheriting the parent test process's routing policy.
			process.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + opts.Home, "KATA_EXTRA_MANAGED_REMOTE_FIXTURE=1"}
			for _, raw := range settings["managed_hooks_env_vars"].([]any) {
				name := raw.(string)
				if value, exists := launching[name]; exists {
					process.Env = append(process.Env, name+"="+value)
				}
			}
			payload, err := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": "native-session", "cwd": opts.Dir})
			require.NoError(t, err)
			process.Stdin = strings.NewReader(string(payload))
			output, err := process.CombinedOutput()
			if !approved {
				require.Error(t, err)
				require.Contains(t, string(output), "allow_insecure")
				return
			}
			require.NoError(t, err, string(output))
			require.JSONEq(t, `{"base_url":"http://daemon.example:7777","allow_insecure":true}`, string(output))
		})
	}
}

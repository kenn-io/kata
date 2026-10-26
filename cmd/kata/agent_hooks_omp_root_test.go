package main

import (
	"context"
	"encoding/json/v2"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func fakeOmp(t *testing.T, output string, err error) {
	t.Helper()
	previous := runOmp
	runOmp = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		require.Equal(t, []string{"config", "path"}, args)
		return []byte(output), err
	}
	t.Cleanup(func() { runOmp = previous })
}
func TestOmpUserRootFromOmpConfigPath(t *testing.T) {
	home := isolateAgentHookHomes(t)
	root := t.TempDir()
	previous := runOmp
	runOmp = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		require.Equal(t, home, dir)
		require.Equal(t, []string{"config", "path"}, args)
		return []byte(root + "\n"), nil
	}
	t.Cleanup(func() { runOmp = previous })
	got, err := agentHookScopePath("omp", "user", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "extensions", "kata.js"), got)
}
func TestOmpUserRootRejectsRelativeOrEmpty(t *testing.T) {
	for _, out := range []string{"", "agent", "warn\n\n", "/valid/agent\nrelative\n"} {
		t.Run(out, func(t *testing.T) {
			fakeOmp(t, out, nil)
			_, err := resolveOmpUserRoot(context.Background(), t.TempDir())
			require.ErrorIs(t, err, errOmpUnavailable)
		})
	}
}
func TestOmpUnavailableExplicitTargetErrors(t *testing.T) {
	isolateAgentHookHomes(t)
	fakeOmp(t, "", exec.ErrNotFound)
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp")
	require.EqualError(t, err, "omp not found on PATH; install omp or use --scope project")
}
func TestOmpUnavailableAutomaticSkip(t *testing.T) {
	isolateAgentHookHomes(t)
	fakeOmp(t, "", exec.ErrNotFound)
	targets, err := selectNativeAgentHookTargets(nil, true, "user", "", t.TempDir())
	require.NoError(t, err)
	for _, target := range targets {
		if target.capability.Name == "omp" {
			require.Equal(t, "omp unavailable", target.skip)
			return
		}
	}
	t.Fatal("omp missing")
}
func TestOmpCanonicalNames(t *testing.T) {
	for _, name := range []string{"omp", "OMP", "oh-my-pi"} {
		got, err := canonicalNativeAttentionTarget(name)
		require.NoError(t, err)
		require.Equal(t, "omp", got)
	}
}
func TestAgentHookContractOmpPointsToInstall(t *testing.T) {
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "contract", "omp")
	require.EqualError(t, err, "omp uses a native extension; install it with kata agent-hook install omp")
}
func TestOmpRejectsConfigFlag(t *testing.T) {
	isolateAgentHookHomes(t)
	resetFlags(t)
	_, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "install", "omp", "--scope", "project", "--workspace", t.TempDir(), "--config", filepath.Join(t.TempDir(), "config"))
	require.ErrorContains(t, err, "--config is unsupported")
}
func TestOmpUserRootPreservesPathWhitespace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent ")
	fakeOmp(t, "note\n"+root+"\n \t\n", nil)
	got, err := resolveOmpUserRoot(context.Background(), t.TempDir())
	require.NoError(t, err)
	require.Equal(t, root, got)
}
func TestOmpUnavailableBareStatusSkips(t *testing.T) {
	isolateAgentHookHomes(t)
	fakeOmp(t, "", exec.ErrNotFound)
	resetFlags(t)
	out, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "--json")
	require.NoError(t, err)
	var report agentHookStatusReport
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	for _, row := range report.Harnesses {
		if row.Harness == "omp" {
			require.Equal(t, "skipped", row.State)
			require.Equal(t, "omp unavailable", row.Reason)
			resetFlags(t)
			text, _, err := executeAgentHook(t, strings.NewReader(""), "agent-hook", "status", "--scope", "user")
			require.NoError(t, err)
			require.Contains(t, text, "omp (user): skipped: omp unavailable")
			return
		}
	}
	t.Fatal("missing skipped omp status")
}

func TestOmpProjectAutomaticDoesNotProbe(t *testing.T) {
	isolateAgentHookHomes(t)
	previous := runOmp
	runOmp = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("project scope probed omp")
		return nil, nil
	}
	t.Cleanup(func() { runOmp = previous })
	targets, err := selectNativeAgentHookTargets(nil, true, "project", "", t.TempDir())
	require.NoError(t, err)
	for _, target := range targets {
		if target.capability.Name == "omp" {
			require.Equal(t, "not configured", target.skip)
			return
		}
	}
	t.Fatal("missing omp target")
}
func TestOmpNativeRegistration(t *testing.T) {
	capability, err := lookupAgentHookCapability("oh-my-pi")
	require.NoError(t, err)
	require.Equal(t, agentHookCapability{Name: "omp", Contract: true, AttentionStart: true, AttentionEnd: true, Project: true, Note: "omp >=18.6.3; restart omp after installation. Offline status cannot confirm runtime loading."}, capability)
	root := t.TempDir()
	require.Equal(t, root, nativeAgentHookConfigRoot("omp", "user", filepath.Join(root, "extensions", "kata.js")))
	contracts, _ := agentHookHarnessCompletion(nil, nil, "om")
	require.Empty(t, contracts)
	installs, _ := agentHookUserCompletion(false)(nil, nil, "om")
	require.Equal(t, []string{"omp"}, installs)
}

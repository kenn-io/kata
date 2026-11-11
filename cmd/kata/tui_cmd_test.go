package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/tui"
)

// kata tui needs a TTY, so we exercise the registration via --help;
// cobra prints help text and returns before RunE is invoked.
//
// --all and --include-deleted are intentionally NOT
// registered: the daemon has no cross-project list endpoint and no
// include_deleted query param, so either flag would advertise a
// capability the wire cannot deliver. Both gates land at the daemon
// boundary; re-add when handlers_issues.go grows the routes.
func TestTUI_CommandRegistered(t *testing.T) {
	out, err := runCmdOutput(t, nil, "tui", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "--uid-format") {
		t.Fatalf("--uid-format missing from help: %s", out)
	}
	if !strings.Contains(out, "--mouse") {
		t.Fatalf("--mouse missing from help: %s", out)
	}
	if !strings.Contains(out, "kata tui [issue-ref]") {
		t.Fatalf("optional issue ref missing from help: %s", out)
	}
	for _, banned := range []string{"--all", "--include-deleted"} {
		if strings.Contains(out, banned) {
			t.Fatalf("%s leaked back into help (daemon support not yet wired): %s",
				banned, out)
		}
	}
}

func TestTUI_RejectsInvalidUIDFormatBeforeTTYCheck(t *testing.T) {
	_, err := runCmdOutput(t, nil, "tui", "--uid-format", "wide")
	if err == nil {
		t.Fatal("expected invalid uid format error")
	}
	if !strings.Contains(err.Error(), "uid format must be one of none, short, full") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTUI_RejectsAgentOutputBeforeLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"--agent", "tui"},
		{"tui", "--agent"},
		{"--format", "agent", "tui"},
		{"tui", "--format", "agent"},
	} {
		stdout, stderr, err := executeRootCapture(t, t.Context(), args...)
		require.Error(t, err, "args %v", args)
		ce := requireCLIError(t, err, ExitUsage)
		assert.Equal(t, kindUsage, ce.Kind)
		assert.Contains(t, ce.Message, "kata tui does not support --agent")
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "kata tui does not support --agent")
		assert.NotContains(t, stderr, "terminal")
	}
}

func TestTUI_AcceptsOptionalIssueRef(t *testing.T) {
	var got tui.Options
	old := runTUI
	runTUI = func(_ context.Context, opts tui.Options) error {
		got = opts
		return nil
	}
	t.Cleanup(func() { runTUI = old })

	_, err := runCmdOutput(t, nil, "tui", "abc4")
	require.NoError(t, err)
	assert.Equal(t, "abc4", got.InitialIssueRef)
}

func TestTUI_ThreadsProjectAndWorkspaceSelectors(t *testing.T) {
	var got tui.Options
	old := runTUI
	runTUI = func(_ context.Context, opts tui.Options) error {
		got = opts
		return nil
	}
	t.Cleanup(func() { runTUI = old })
	workspace := t.TempDir()

	_, err := runCmdOutput(
		t, nil,
		"--project", "project-b",
		"--workspace", workspace,
		"tui", "abc4",
	)

	require.NoError(t, err)
	assert.Equal(t, "project-b", got.ProjectName)
	assert.Equal(t, workspace, got.Workspace)
	assert.Equal(t, "abc4", got.InitialIssueRef)
}

// TestTUI_RejectsMoreThanOneIssueRef guards the one-ref CLI contract:
// accepting two refs would make the direct-open target ambiguous.
func TestTUI_RejectsMoreThanOneIssueRef(t *testing.T) {
	_, err := runCmdOutput(t, nil, "tui", "abc4", "def5")
	if err == nil {
		t.Fatal("expected error for more than one issue ref")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown command") &&
		!strings.Contains(msg, "accepts at most 1 arg") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTUI_ConfirmQuitConfigAccepted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nconfirm_quit = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	run := false
	old := runTUI
	runTUI = func(_ context.Context, _ tui.Options) error {
		run = true
		return nil
	}
	t.Cleanup(func() { runTUI = old })

	_, err := runCmdOutput(t, nil, "tui")
	require.NoError(t, err)
	assert.True(t, run, "runTUI hook did not run")
}

func TestTUI_ConfirmQuitConfigModes(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		wantSkip  bool
		wantMouse bool
	}{
		{name: "missing config"},
		{name: "missing key", config: "[tui]\nmouse = true\n", wantMouse: true},
		{name: "true", config: "[tui]\nconfirm_quit = true\n"},
		{name: "false", config: "[tui]\nconfirm_quit = false\n", wantSkip: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			if tt.config != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var got tui.Options
			old := runTUI
			runTUI = func(_ context.Context, opts tui.Options) error {
				got = opts
				return nil
			}
			t.Cleanup(func() { runTUI = old })

			_, err := runCmdOutput(t, nil, "tui")
			require.NoError(t, err)
			assert.Equal(t, tt.wantSkip, got.SkipQuitConfirm)
			assert.Equal(t, tt.wantMouse, got.Mouse)
		})
	}
}

func TestTUI_MouseOptionReadsConfigToml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nmouse = true\n"+untrustedWebConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newTUICmd()
	got, err := resolveTUIConfig(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Mouse {
		t.Fatal("mouse option = false, want true from [tui] mouse")
	}
}

func TestTUI_MouseFlagOverridesConfigToml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nmouse = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newTUICmd()
	if err := cmd.Flags().Set("mouse", "true"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveTUIConfig(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Mouse {
		t.Fatal("mouse option = false, want true from --mouse")
	}
}

// A failed TUI boot must give recovery advice for the selected local profile.
func TestTUI_UnreachableBootUsesSelectedProfile(t *testing.T) {
	for _, route := range []string{"explicit", "workspace", "active", "routing changed", "inspection failure"} {
		t.Run(route, func(t *testing.T) {
			home, workspace, uid, _ := stoppedProfileCLI(t)
			if route == "active" {
				require.NoError(t, os.Remove(filepath.Join(workspace, ".kata.local.toml")))
				personal := t.TempDir()
				t.Setenv("KATA_HOME", personal)
				catalog := fmt.Sprintf("active_daemon = \"work\"\n[[daemon]]\nname = \"work\"\nlocal = true\nhome = %q\ninstance_uid = %q\n", home, uid)
				require.NoError(t, os.WriteFile(filepath.Join(personal, "config.toml"), []byte(catalog), 0600))
			}
			if route == "inspection failure" {
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte("invalid = ["), 0600))
			}
			bootErr := &client.LocalDaemonUnreachableError{PID: 42, Address: "127.0.0.1:1", DataDir: t.TempDir()}
			old := runTUI
			var receivedName string
			runTUI = func(_ context.Context, opts tui.Options) error {
				receivedName = opts.DaemonName
				if route == "routing changed" {
					require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"), []byte("[server]\ndaemon = \"other\"\n"), 0600))
				}
				return bootErr
			}
			t.Cleanup(func() { runTUI = old })
			args := []string{"--workspace", workspace}
			if route == "explicit" {
				args = append(args, "--daemon", "work")
			}
			args = append(args, "tui")
			_, _, err := executeRootCapture(t, t.Context(), args...)
			if route == "inspection failure" {
				require.Empty(t, receivedName)
				require.ErrorIs(t, err, bootErr)
				require.Same(t, bootErr, err)
				return
			}
			require.Equal(t, "work", receivedName)
			ce := requireCLIError(t, err, ExitDaemonUnavail)
			require.Contains(t, ce.Hint, "--daemon=work")
			require.NotContains(t, ce.Hint, "other")
			require.Contains(t, ce.Hint, "daemon")
			require.Contains(t, ce.Hint, "recover")
		})
	}
}

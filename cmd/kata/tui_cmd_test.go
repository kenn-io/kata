package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kata tui needs a TTY, so we exercise the registration via --help;
// cobra prints help text and returns before RunE is invoked.
//
// --all-projects and --include-deleted are intentionally NOT
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
	for _, banned := range []string{"--all-projects", "--include-deleted"} {
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
		stdout, stderr, err := executeRootCapture(t, context.Background(), args...)
		require.Error(t, err, "args %v", args)
		ce := requireCLIError(t, err, ExitUsage)
		assert.Equal(t, kindUsage, ce.Kind)
		assert.Contains(t, ce.Message, "kata tui does not support --agent")
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "kata tui does not support --agent")
		assert.NotContains(t, stderr, "terminal")
	}
}

// TestTUI_RejectsExtraArgs guards the cobra.NoArgs constraint: a typo'd
// positional must error out before RunE so the user sees a usage
// failure (and the TTY check in tui.Run is never reached, which would
// be inappropriate for an arg-parse failure).
func TestTUI_RejectsExtraArgs(t *testing.T) {
	_, err := runCmdOutput(t, nil, "tui", "extra-positional")
	if err == nil {
		t.Fatal("expected error for extra positional arg")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown command") &&
		!strings.Contains(msg, "accepts no args") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTUI_MouseOptionReadsConfigToml(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nmouse = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newTUICmd()
	got, err := resolveTUIMouseOption(cmd, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
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
	got, err := resolveTUIMouseOption(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("mouse option = false, want true from --mouse")
	}
}

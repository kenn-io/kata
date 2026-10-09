package main

import (
	"context"
	"strings"
	"testing"
)

func TestHelpRootWorkflow(t *testing.T) {
	out, stderr, err := executeRootCapture(t, context.Background(), "--help")
	if err != nil || stderr != "" {
		t.Fatalf("root help: %v, %s", err, stderr)
	}
	for _, fact := range []string{"Agent workflow", "kata quickstart --format contract", "Exit codes:", "project#abc4", "KATA_INBOX_USER", "agent output: one OK/ERR line", "full JSON envelope for scripts", "named daemon catalog entry", ".kata.toml at or above --workspace"} {
		if !strings.Contains(out, fact) {
			t.Errorf("root help missing %q", fact)
		}
	}
	previous := -1
	for _, title := range []string{"Find and read work:", "Change issues:", "Coordinate agents:", "Set up and diagnose:", "Administer (rarely needed by agents):"} {
		at := strings.Index(out, title)
		if at <= previous {
			t.Errorf("missing or out-of-order group %q", title)
		}
		previous = at
	}
}

func TestHelpCompactGlobalFlags(t *testing.T) {
	out, _, err := executeRootCapture(t, context.Background(), "create", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Global: --agent") || strings.Contains(out, "Global Flags:") {
		t.Errorf("inherited flags are not compact:\n%s", out)
	}
	for _, flag := range []string{"--as <string>", "--daemon <string>", "--format <string>", "--json", "--project <string>", "-q/--quiet", "--teammate <string>", "--workspace <string>", "Details: kata --help"} {
		if !strings.Contains(out, flag) {
			t.Errorf("compact globals missing %q", flag)
		}
	}
}

func TestHelpCoreConventions(t *testing.T) {
	for _, tc := range []struct {
		command string
		facts   []string
	}{
		{"create", []string{"seven days", "open or closed"}},
		{"ready", []string{"Most recently updated first", "kata next selects by priority"}},
		{"meta", []string{"work.attention_msg", "someday", "needs-human", "stuck", "work.branch"}},
		{"schedule", []string{"YYYY-MM-DD", "monday", "kata ready", "someday"}},
		{"inbox", []string{"--context conflicts with --agent, --json, or --format"}},
		{"notify", []string{"<actor>/<teammate>", "required unless --clear"}},
		{"close", []string{"40 chars", "wontfix 60"}},
		{"delete", []string{"--force", `--confirm "DELETE example-project#abc4"`}},
	} {
		out, _, err := executeRootCapture(t, context.Background(), tc.command, "--help")
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range tc.facts {
			if !strings.Contains(out, fact) {
				t.Errorf("%s help missing %q", tc.command, fact)
			}
		}
	}
}

func TestHelpActorGuidanceDistinguishesClientAndDaemonIdentity(t *testing.T) {
	for _, tc := range []struct {
		command string
		facts   []string
	}{
		{"whoami", []string{"client-resolved actor", "does not query daemon authentication", "kata status <ref>"}},
		{"claim", []string{"daemon's authenticated actor takes precedence", "kata status <ref>", "kata whoami shows only the client-selected actor"}},
		{"status", []string{"current owner", "effective actor"}},
	} {
		out, _, err := executeRootCapture(t, context.Background(), tc.command, "--help")
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range tc.facts {
			if !strings.Contains(out, fact) {
				t.Errorf("%s help missing %q", tc.command, fact)
			}
		}
	}
}

func TestHelpLabelRemoveAlias(t *testing.T) {
	if err := validateHelpCommand("kata label remove abc4 needs-review --agent"); err != nil {
		t.Fatal(err)
	}
}

// The required help copy exceeds the original 5500/2000-byte targets.
// These caps retain that copy and leave a small margin for future wording.
func TestHelpSizeBudget(t *testing.T) {
	for _, tc := range []struct {
		args []string
		max  int
	}{
		{[]string{"--help"}, 6200},
		{[]string{"create", "--help"}, 2600},
	} {
		out, _, err := executeRootCapture(t, context.Background(), tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > tc.max {
			t.Errorf("%v: %d bytes exceeds %d", tc.args, len(out), tc.max)
		}
	}
}

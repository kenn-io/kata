package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Help examples are copied verbatim by agents, so every advertised kata
// command must still parse: the subcommand exists, the flags exist, and the
// positional argument count is accepted. Nothing is executed.
func TestHelpExamplesParse(t *testing.T) {
	root := newRootCmd()
	walkVisibleHelp(root, func(cmd *cobra.Command) {
		for i, source := range []string{cmd.Long, cmd.Example} {
			lines, err := helpCommandLines(source, i == 0)
			if err != nil {
				t.Errorf("%s: %v", cmd.CommandPath(), err)
				continue
			}
			for _, line := range lines {
				if err := validateHelpCommand(line); err != nil {
					t.Errorf("%s: %q: %v", cmd.CommandPath(), line, err)
				}
			}
		}
	})
	for _, text := range []string{agentContractText, agentQuickstartText, agentQuickstartCompactText, hooklessQuickstartText} {
		lines, err := helpCommandLines(text, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range lines {
			if err := validateHelpCommand(line); err != nil {
				t.Errorf("agent guidance: %q: %v", line, err)
			}
		}
	}
	out, _, err := executeRootCapture(t, context.Background(), "quickstart")
	if err != nil {
		t.Fatal(err)
	}
	lines, err := helpCommandLines(out, true)
	if err != nil || len(lines) == 0 {
		t.Fatalf("quickstart commands: %v, %v", lines, err)
	}
	for _, line := range lines {
		if err := validateHelpCommand(line); err != nil {
			t.Errorf("quickstart: %q: %v", line, err)
		}
	}
}

// Keeps TestHelpExamplesParse from passing vacuously. The first case is the
// stale close-help advice this check found.
func TestValidateHelpCommandRejectsStaleExamples(t *testing.T) {
	for _, line := range []string{
		`kata edit abc4 --label needs-review`,
		`kata meta made-up`,
		`kata unassign abc4 --expect-owner your actor`,
		`kata comment abc4 -m "unfinished`,
	} {
		if err := validateHelpCommand(line); err == nil {
			t.Errorf("accepted stale example: %s", line)
		}
	}
	for _, line := range []string{
		`kata close abc4 --done -m "a message with  # and > inside"  # note`,
		`kata openapi --format yaml > openapi.yaml`,
	} {
		if err := validateHelpCommand(line); err != nil {
			t.Errorf("rejected valid example %s: %v", line, err)
		}
	}
}

// kataCommandGroups is keyed by command name, so a new or renamed command
// would otherwise fall into an "Additional Commands" section.
func TestHelpTopLevelCommandsHaveGroups(t *testing.T) {
	for _, cmd := range newRootCmd().Commands() {
		if cmd.IsAvailableCommand() && cmd.GroupID == "" {
			t.Errorf("%s has no help group in kataCommandGroups", cmd.Name())
		}
	}
}

func TestKataGlobalFlags(t *testing.T) {
	root := &cobra.Command{Use: "kata"}
	root.PersistentFlags().Bool("agent", false, "")
	root.PersistentFlags().BoolP("quiet", "q", false, "")
	root.PersistentFlags().String("project", "", "")
	root.PersistentFlags().String("internal", "", "")
	if err := root.PersistentFlags().MarkHidden("internal"); err != nil {
		t.Fatal(err)
	}
	child := &cobra.Command{Use: "create"}
	root.AddCommand(child)
	if got, want := kataGlobalFlags(child), "--agent, --project <string>, -q/--quiet"; got != want {
		t.Fatalf("kataGlobalFlags = %q, want %q", got, want)
	}
}

func walkVisibleHelp(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Hidden {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkVisibleHelp(child, visit)
	}
}

// helpCommandLines returns the kata commands in help text, joining trailing
// backslash continuations. Long text only counts indented lines as commands.
func helpCommandLines(text string, indented bool) ([]string, error) {
	var commands []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if indented && !strings.HasPrefix(lines[i], "  ") {
			continue
		}
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "kata ") {
			continue
		}
		for strings.HasSuffix(line, "\\") {
			if i+1 == len(lines) {
				return nil, fmt.Errorf("unfinished continuation: %s", line)
			}
			i++
			line = strings.TrimSuffix(line, "\\") + strings.TrimSpace(lines[i])
		}
		commands = append(commands, line)
	}
	return commands, nil
}

func validateHelpCommand(line string) error {
	argv, err := splitHelpWords(line)
	if err != nil {
		return err
	}
	if len(argv) == 0 || argv[0] != "kata" {
		return fmt.Errorf("expected kata command")
	}
	root := newRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	cmd, rest, err := root.Find(argv[1:])
	if err != nil {
		return err
	}
	cmd.InitDefaultHelpFlag()
	if err := cmd.ParseFlags(rest); err != nil {
		return err
	}
	args := cmd.Flags().Args()
	// Find stops at the deepest known command, leaving a mistyped
	// subcommand as a positional argument of a non-runnable parent.
	if (!cmd.Runnable() || cmd == root) && len(args) > 0 {
		return fmt.Errorf("unresolved command path: %v", args)
	}
	return cmd.ValidateArgs(args)
}

// splitHelpWords splits the shell notation used in help: single and double
// quotes, with an unquoted "#", ">", or "|" after a space ending the command.
// Help text does not use backslash escapes inside a line.
func splitHelpWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	started := false
	for i, ch := range line {
		switch {
		case quote != 0 && ch == quote:
			quote = 0
		case quote != 0:
			word.WriteRune(ch)
		case ch == '\'' || ch == '"':
			quote, started = ch, true
		case strings.ContainsRune("#>|", ch) && i > 0 && line[i-1] == ' ':
			return words, nil
		case ch == ' ' || ch == '\t':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

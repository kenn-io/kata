package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// These checks parse advertised commands without running them or contacting a daemon.
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
	for _, args := range [][]string{{"quickstart"}, {"quickstart", "--agent"}} {
		out, _, err := executeRootCapture(t, context.Background(), args...)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := helpCommandLines(out, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) == 0 && len(args) == 1 {
			t.Fatal("quickstart checked no commands")
		}
		for _, line := range lines {
			if err := validateHelpCommand(line); err != nil {
				t.Errorf("quickstart: %q: %v", line, err)
			}
		}
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
	if (!cmd.Runnable() || cmd == root) && len(args) > 0 {
		return fmt.Errorf("unresolved command path: %v", args)
	}
	return cmd.ValidateArgs(args)
}

// splitHelpWords supports the shell notation used in help, without executing it.
// An unquoted terminal output redirection belongs to the shell, not Cobra.
func splitHelpWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote byte
	started := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if ch == '\\' && quote == '"' {
				if i+1 == len(line) {
					return nil, fmt.Errorf("unfinished escape")
				}
				next := line[i+1]
				if strings.ContainsRune(`\"$`+"`", rune(next)) {
					i++
					ch = next
				}
			}
			word.WriteByte(ch)
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			started = true
		case '\\':
			if i+1 == len(line) {
				return nil, fmt.Errorf("unfinished escape")
			}
			i++
			word.WriteByte(line[i])
			started = true
		case ' ', '\t':
			flush()
		case '#':
			if i >= 2 && line[i-2:i] == "  " {
				flush()
				return words, nil
			}
			word.WriteByte(ch)
			started = true
		case '>':
			if i > 0 && (line[i-1] == ' ' || line[i-1] == '\t') {
				flush()
				return words, nil
			}
			word.WriteByte(ch)
			started = true
		case '|':
			if i > 0 && i+1 < len(line) && line[i-1] == ' ' && line[i+1] == ' ' {
				flush()
				return words, nil
			}
			word.WriteByte(ch)
			started = true
		case '2':
			if !started && i+1 < len(line) && line[i+1] == '>' {
				flush()
				return words, nil
			}
			word.WriteByte(ch)
			started = true
		default:
			word.WriteByte(ch)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	flush()
	return words, nil
}

func TestHelpExampleParser(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{`kata comment abc4 -m "two  # literal words" --agent  # note`, []string{"kata", "comment", "abc4", "-m", "two  # literal words", "--agent"}},
		{`kata comment abc4 -m 'it'\''s fixed'`, []string{"kata", "comment", "abc4", "-m", "it's fixed"}},
		{`kata --teammate='' whoami --agent`, []string{"kata", "--teammate=", "whoami", "--agent"}},
		{`kata openapi --format yaml > openapi.yaml`, []string{"kata", "openapi", "--format", "yaml"}},
		{`kata openapi >> schema.yaml | cat`, []string{"kata", "openapi"}},
		{`kata openapi 2> errors.log`, []string{"kata", "openapi"}},
		{`kata openapi | cat`, []string{"kata", "openapi"}},
		{`kata comment abc4 -m ">"`, []string{"kata", "comment", "abc4", "-m", ">"}},
		{`kata create <title> --meta work.branch=<branch>`, []string{"kata", "create", "<title>", "--meta", "work.branch=<branch>"}},
	} {
		got, err := splitHelpWords(tc.line)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: %v, %v; want %v", tc.line, got, err, tc.want)
		}
	}
	for _, line := range []string{`kata edit abc4 --label bug`, `kata meta made-up`, `kata label add abc4`, `kata comment abc4 -m "unfinished`} {
		if err := validateHelpCommand(line); err == nil {
			t.Errorf("accepted invalid example: %s", line)
		}
	}
	for _, line := range []string{`kata --help`, `kata help create`, `kata meta --help`, `kata --agent label rm abc4 bug`, `kata agent-instructions --format contract`} {
		if err := validateHelpCommand(line); err != nil {
			t.Errorf("rejected %s: %v", line, err)
		}
	}
	lines, err := helpCommandLines("kata close abc4 \\\n  --done -m 'a message'  # note\nprose", false)
	if err != nil || len(lines) != 1 {
		t.Fatalf("continued command: %v, %v", lines, err)
	}
	if err := validateHelpCommand(lines[0]); err != nil {
		t.Fatal(err)
	}
}

func FuzzHelpWordsRoundTrip(f *testing.F) {
	for _, s := range []string{"", "two  # words", "quote' and \\", "<actor>/<teammate>", "\x00\xff"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		quoted := "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
		got, err := splitHelpWords("kata comment abc4 -m " + quoted)
		want := []string{"kata", "comment", "abc4", "-m", value}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip: %q: %q, %v", value, got, err)
		}
	})
}

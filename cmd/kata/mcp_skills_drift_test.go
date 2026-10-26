package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcpserver "go.kenn.io/kata/internal/mcp"
)

func TestExtractKataCommands(t *testing.T) {
	valueFlags := map[string]bool{"teammate": true, "project": true}
	for input, want := range map[string][]string{
		`kata --teammate=teammate-2 comment abc4 --body "x"`:    {"kata comment"},
		`kata --teammate='' comment abc4`:                       {"kata comment"},
		`kata --teammate teammate-2 --agent comment abc4`:       {"kata comment"},
		`tracked:\nkata claim <ref>\nkata meta set <ref> x`:     {"kata claim", "kata meta set"},
		`kata --project foo search "login race"`:                {"kata search"},
		`kata search --project foo "login race" --agent`:        {"kata search"},
		`kata label add abc4 safari`:                            {"kata label add"},
		`kata frobnicate now`:                                   {"kata frobnicate now"},
		`Use kata as the shared issue ledger`:                   {"kata as the"},
		`see kata#abc4 and Kata is`:                             nil,
		"Run `kata delete` or `kata purge`.":                    {"kata delete", "kata purge"},
		`close [label="kata close <ref>\nkata meta set <ref>"]`: {"kata close", "kata meta set"},
		`kata --project`:                                        nil,
		`kata`:                                                  nil,
	} {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, extractKataCommands(input, valueFlags))
		})
	}
}

func TestMCPSkillGuidesMatchCLI(t *testing.T) {
	root := newRootCmd()
	valueFlags := map[string]bool{}
	root.PersistentFlags().VisitAll(func(flag *pflag.Flag) {
		valueFlags[flag.Name] = flag.Value.Type() != "bool"
	})
	rows := mcpserver.SkillCommands()
	rowKeys := map[string]bool{}
	for _, row := range rows {
		assert.False(t, rowKeys[row.CLI], "duplicate command row %q", row.CLI)
		rowKeys[row.CLI] = true
		command, rest, err := root.Find(strings.Fields(row.CLI)[1:])
		require.NoError(t, err, row.CLI)
		assert.Empty(t, rest, row.CLI)
		assert.Equal(t, row.CLI, command.CommandPath())
	}

	usedProse := map[string]bool{}
	for _, name := range mcpserver.SkillGuides() {
		t.Run(name, func(t *testing.T) {
			source, ok := mcpserver.SkillSource(name)
			require.True(t, ok)
			mentions := extractKataCommands(source, valueFlags)
			for _, mention := range mentions {
				word := strings.Fields(mention)[1]
				if guideProseWords[word] {
					usedProse[word] = true
				}
			}
			resolved, unresolved := resolveMentions(mentions, rows)
			for _, mention := range unresolved {
				t.Errorf("guide %s mentions %q with no command row", name, mention)
			}
			declared := mcpserver.SkillGuideCommands(name)
			seen := map[string]bool{}
			for _, key := range declared {
				assert.True(t, rowKeys[key], "guide declares unknown command %q", key)
				assert.False(t, seen[key], "guide declares duplicate command %q", key)
				seen[key] = true
			}
			for _, key := range resolved {
				assert.Contains(t, declared, key, "mentioned command missing from guide table")
			}
			// Workflow intentionally carries the entire table for its shorter contract.
			if name != "workflow" {
				assert.ElementsMatch(t, declared, resolved)
			}
		})
	}
	for word := range guideProseWords {
		assert.True(t, usedProse[word], "unused prose stop word %q", word)
	}
}

func TestMCPSkillDriftCatchesUnmappedCommand(t *testing.T) {
	mentions := extractKataCommands("kata frobnicate x", nil)
	resolved, unresolved := resolveMentions(mentions, mcpserver.SkillCommands())
	assert.Empty(t, resolved)
	assert.Equal(t, []string{"kata frobnicate x"}, unresolved)
}

func TestMCPSkillDriftResolvesLongestCommandPrefix(t *testing.T) {
	rows := []mcpserver.SkillCommand{{CLI: "kata label"}, {CLI: "kata label add"}, {CLI: "kata search"}}
	resolved, unresolved := resolveMentions([]string{
		"kata label add", "kata search text", "kata label add", "kata searcher", "kata as the",
	}, rows)
	assert.Equal(t, []string{"kata label add", "kata search"}, resolved)
	assert.Equal(t, []string{"kata searcher"}, unresolved)
}

// These words follow "kata" in the served prose, rather than naming commands.
// TestMCPSkillGuidesMatchCLI requires each exception to remain in use.
var guideProseWords = map[string]bool{
	"agent": true,
	"as":    true,
	"for":   true,
	"state": true,
}

var guideCommandWord = regexp.MustCompile(`^[a-z][a-z-]*$`)

// extractKataCommands recognizes mentions without consulting the command tree.
// valueFlags only describes root flags, so a removed or misspelled command
// remains a candidate for the mapping check.
func extractKataCommands(text string, valueFlags map[string]bool) []string {
	words := strings.Fields(strings.ReplaceAll(text, `\n`, " "))
	var commands []string
	for i, token := range words {
		token = strings.TrimPrefix(token, "`")
		token = strings.TrimPrefix(token, `[label="`)
		if token != "kata" {
			continue
		}
		j := i + 1
		for j < len(words) && strings.HasPrefix(words[j], "--") {
			flag, _, inlineValue := strings.Cut(strings.TrimPrefix(words[j], "--"), "=")
			j++
			if !inlineValue && valueFlags[flag] {
				j++
			}
		}
		path := []string{"kata"}
		for j < len(words) && len(path) < 3 {
			raw := words[j]
			word := strings.TrimRight(raw, "`.,;:!?)]}\"")
			if !guideCommandWord.MatchString(word) {
				break
			}
			path = append(path, word)
			j++
			// A closing code span, sentence or dot label ends this mention.
			if raw != word {
				break
			}
		}
		if len(path) > 1 {
			commands = append(commands, strings.Join(path, " "))
		}
	}
	return commands
}

func resolveMentions(mentions []string, rows []mcpserver.SkillCommand) (resolved []string, unresolved []string) {
	seen := map[string]bool{}
	for _, mention := range mentions {
		if guideProseWords[strings.Fields(mention)[1]] {
			continue
		}
		longest := ""
		for _, row := range rows {
			if (mention == row.CLI || strings.HasPrefix(mention, row.CLI+" ")) && len(row.CLI) > len(longest) {
				longest = row.CLI
			}
		}
		if longest == "" {
			unresolved = append(unresolved, mention)
		} else if !seen[longest] {
			seen[longest] = true
			resolved = append(resolved, longest)
		}
	}
	return resolved, unresolved
}

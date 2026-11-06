package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/clisearch"
	"hegel.dev/go/hegel"
)

var cliSearchTasks = []struct{ query, command string }{
	{"add a comment to an issue", "kata comment"},
	{"log a note on a ticket", "kata comment"},
	{"create a bug", "kata create"},
	{"find issues by title", "kata search"},
	{"list open issues", "kata list"},
	{"show issue details", "kata show"},
	{"claim a ticket", "kata claim"},
	{"find unblocked work", "kata ready"},
	{"close a completed issue", "kata close"},
	{"set issue metadata", "kata meta set"},
	{"notify a teammate", "kata notify"},
	{"list attention requests", "kata inbox"},
	{"stream issue events", "kata events"},
	{"show daemon logs", "kata daemon logs"},
	{"list scheduled jobs", "kata cron job list"},
	{"show a recorded run", "kata cron run show"},
	{"show issue claim status", "kata status"},
}

// These tasks are the product contract from the reviewed slice design. Keep
// their wording stable so changes to ranking have a comparable evaluation.
func TestCLISearchTasks(t *testing.T) {
	for _, task := range cliSearchTasks {
		t.Run(task.query, func(t *testing.T) {
			out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", task.query, "--json")
			require.NoError(t, err, stderr)
			var response struct {
				Results []struct{ Command string } `json:"results"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &response))
			require.NotEmpty(t, response.Results)
			require.Equal(t, task.command, response.Results[0].Command, out)
			for _, result := range response.Results {
				require.False(t, result.Command == "kata" || result.Command == "kata ui" || result.Command == "kata tui", out)
				require.NotContains(t, result.Command, "kata cli")
				require.NotContains(t, result.Command, "kata connector serve")
				require.NotContains(t, result.Command, "kata mcp serve")
				require.NotContains(t, result.Command, "kata agent-hook attention")
				require.NotEqual(t, "kata agent-hook contract", result.Command)
				require.NotEqual(t, "kata attention-hook", result.Command)
				require.NotEqual(t, "kata agent-contract-hook", result.Command)
			}
		})
	}
}

// Contract: command discovery works before configuring a daemon or workspace,
// while retaining Kata's existing output selectors and JSON version envelope.
func TestCLISearchOffline(t *testing.T) {
	home := t.TempDir()
	state := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("KATA_HOME", state)
	t.Setenv("KATA_DB", filepath.Join(state, "missing", "kata.db"))
	t.Setenv("KATA_SERVER", "invalid://no-daemon")
	t.Setenv("KATA_INBOX_USER", "operator/agent")
	t.Setenv("KATA_HOOK_VERSION", "1")
	require.NoError(t, os.WriteFile(filepath.Join(state, "config.toml"), []byte("not valid TOML ["), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"), []byte("not valid TOML ["), 0o600))
	t.Chdir(workspace)

	out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", "add a comment to an issue", "--json",
		"--daemon", "missing-daemon", "--workspace", filepath.Join(workspace, "missing"))
	require.NoError(t, err, stderr)
	require.Empty(t, stderr)
	var response struct {
		Version int    `json:"kata_api_version"`
		Query   string `json:"query"`
		Results []struct {
			Command string `json:"command"`
			Summary string `json:"summary"`
			Usage   string `json:"usage"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &response))
	require.Equal(t, 1, response.Version)
	require.Equal(t, "add a comment to an issue", response.Query)
	require.NotEmpty(t, response.Results)
	require.LessOrEqual(t, len(response.Results), 5)
	require.Equal(t, "kata comment", response.Results[0].Command)
	require.Equal(t, "append a comment to an issue", response.Results[0].Summary)
	require.Equal(t, "kata comment <issue-ref>", response.Results[0].Usage)
	require.Nil(t, cliUseTarget.Load(), "metadata-only commands must never resolve a telemetry target")
	entries, err := os.ReadDir(state)
	require.NoError(t, err)
	require.Len(t, entries, 1, "search must not initialize daemon state")
	require.Equal(t, "config.toml", entries[0].Name())
	entries, err = os.ReadDir(home)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestCLISearchOutputModes(t *testing.T) {
	query := "add a comment to an issue"
	text, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", query)
	require.NoError(t, err, stderr)
	require.Empty(t, stderr)
	require.True(t, strings.HasPrefix(text, "kata comment  append a comment to an issue\n"), text)
	require.True(t, strings.HasSuffix(text, "Run 'kata comment --help' for flags and examples.\n"), text)
	require.Less(t, len(text), 1024)
	for _, selector := range [][]string{{"--agent"}, {"--format", "agent"}} {
		args := append([]string{"cli", "search", query}, selector...)
		out, stderr, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err, stderr)
		require.Empty(t, stderr)
		require.Regexp(t, `^OK search results=\d+\n`, out)
		require.True(t, strings.HasSuffix(out, text))
	}
	quoted, _, err := executeRootCapture(t, t.Context(), "cli", "search", query, "--json")
	require.NoError(t, err)
	words, _, err := executeRootCapture(t, t.Context(), "cli", "search", "add", "a", "comment", "to", "an", "issue", "--format", "json")
	require.NoError(t, err)
	require.JSONEq(t, quoted, words)
}

func TestCLISearchEmptyResults(t *testing.T) {
	out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", "zxqvnonexistent", "--json")
	require.NoError(t, err, stderr)
	require.Empty(t, stderr)
	require.JSONEq(t, `{"kata_api_version":1,"query":"zxqvnonexistent","results":[]}`, out)
	out, stderr, err = executeRootCapture(t, t.Context(), "cli", "search", "zxqvnonexistent")
	require.NoError(t, err, stderr)
	require.Equal(t, "Run 'kata --help' for flags and examples.\n", out)
}

func TestCLISearchUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"cli", "search"},
		{"cli", "search", " \t\n"},
		{"cli", "search", "issue", "--limit", "0"},
		{"cli", "search", "issue", "--limit", "-1"},
		{"cli", "search", "issue", "--limit", "21"},
		{"cli", "search", "issue", "--limit", "no"},
		{"cli", "search", "issue", "--json", "--agent"},
		{"cli", "search", "issue", "--format", "contract"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, stderr, err := executeRootCapture(t, t.Context(), args...)
			require.Error(t, err)
			require.Empty(t, out)
			require.NotEmpty(t, stderr)
			require.Equal(t, ExitUsage, exitCodeForErr(err, runEEntered), stderr)
		})
	}
}

func TestCLISearchPreservesNativeFlagErrors(t *testing.T) {
	for _, args := range [][]string{
		{"cli", "search", "issue", "--unknown-discovery-flag"},
		{"cli", "--unknown-discovery-flag"},
	} {
		_, _, err := executeRootCapture(t, t.Context(), args...)
		require.Error(t, err)
		cli := cliErrorForErr(err, runEEntered)
		require.Equal(t, kindUsage, cli.Kind)
		require.Contains(t, cli.Hint, "--help", "new commands retain native unknown-flag guidance")
	}
	_, _, err := executeRootCapture(t, t.Context(), "cli", "search", "-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "separator")
}

func TestCLISearchWriterErrors(t *testing.T) {
	for _, selector := range [][]string{nil, {"--agent"}, {"--json"}} {
		root := newRootCmd()
		root.SetOut(cliSearchErrorWriter{})
		root.SetArgs(append([]string{"cli", "search", "issue"}, selector...))
		err := root.ExecuteContext(t.Context())
		require.ErrorIs(t, err, errCLISearchWrite)
		require.Equal(t, ExitInternal, exitCodeForErr(err, runEEntered))
	}
}

var errCLISearchWrite = errors.New("fixture output failure")

type cliSearchErrorWriter struct{}

func (cliSearchErrorWriter) Write([]byte) (int, error) { return 0, errCLISearchWrite }

func TestCLISearchHelpDiscovery(t *testing.T) {
	out, stderr, err := executeRootCapture(t, t.Context(), "--help")
	require.NoError(t, err, stderr)
	require.True(t, strings.HasPrefix(out, "Agents: find the right command with: kata cli search \"<what you want to do>\"\n"))
	require.NoError(t, validateHelpCommand(`kata cli search "<what you want to do>"`))
	root := newRootCmd()
	cli := childCommandByName(root, "cli")
	require.NotNil(t, cli)
	require.Equal(t, "find", cli.GroupID)
	out, stderr, err = executeRootCapture(t, t.Context(), "cli")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "search")
	out, stderr, err = executeRootCapture(t, t.Context(), "quickstart")
	require.NoError(t, err, stderr)
	require.Contains(t, out, "1. Find the right command")
	require.Contains(t, out, `kata cli search "<task>"`)
}

func TestCLISearchSkipPolicy(t *testing.T) {
	root := newRootCmd()
	for _, path := range []string{
		"", "tui", "ui", "mcp serve", "connector serve", "connector serve forgejo",
		"agent-hook contract", "agent-hook attention", "agent-hook attention start", "agent-hook attention end",
		"attention-hook", "agent-contract-hook",
	} {
		cmd, rest, err := root.Find(strings.Fields(path))
		require.NoError(t, err)
		require.Empty(t, rest)
		require.True(t, skipCLISearchCommand(cmd), path)
	}
	for _, path := range []string{
		"search", "comment", "comment edit", "daemon start", "daemon stop", "daemon logs",
		"agent-hook install", "agent-hook uninstall", "agent-hook status", "connector list",
	} {
		cmd, rest, err := root.Find(strings.Fields(path))
		require.NoError(t, err)
		require.Empty(t, rest)
		require.False(t, skipCLISearchCommand(cmd), path)
	}
}

func TestCLISearchUsesLiveTree(t *testing.T) {
	root := newRootCmd()
	root.AddCommand(&cobra.Command{
		Use:     "fixture-task",
		Short:   "perform the fixture task",
		GroupID: "find",
		Run: func(_ *cobra.Command, _ []string) {
			t.Fatal("discovery must not execute the recommended command")
		},
	})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"cli", "search", "fixture-task", "--json"})
	require.NoError(t, root.ExecuteContext(t.Context()))
	var response struct {
		Results []struct{ Command string } `json:"results"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.NotEmpty(t, response.Results)
	require.Equal(t, "kata fixture-task", response.Results[0].Command)
}

func TestCLISearchSkipSegmentsAndDescendants(t *testing.T) {
	for _, fixture := range []struct {
		parent, name string
		eligible     bool
	}{
		{"connector serve", "fixture-task", false},
		{"agent-hook attention", "fixture-task", false},
		{"connector", "serve-extra", true},
		{"agent-hook", "attention-extra", true},
		{"fixture-group", "fixture-task", true},
	} {
		t.Run(fixture.parent+" "+fixture.name, func(t *testing.T) {
			root := newRootCmd()
			root.AddCommand(&cobra.Command{Use: "fixture-group", GroupID: "find"})
			parent, rest, err := root.Find(strings.Fields(fixture.parent))
			require.NoError(t, err)
			require.Empty(t, rest)
			parent.AddCommand(&cobra.Command{Use: fixture.name, Short: "fixturezzuniqueword", Run: func(*cobra.Command, []string) {
				t.Fatal("discovery executed its result")
			}})
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs([]string{"cli", "search", "fixturezzuniqueword", "--json"})
			require.NoError(t, root.ExecuteContext(t.Context()))
			var response clisearch.Response
			require.NoError(t, json.Unmarshal(out.Bytes(), &response))
			if fixture.eligible {
				require.Len(t, response.Results, 1)
				require.Equal(t, "kata "+fixture.parent+" "+fixture.name, response.Results[0].Command)
			} else {
				require.Empty(t, response.Results)
			}
		})
	}
}

// Equivalent native selectors must produce identical envelopes for every task,
// including malformed/empty tasks. -- keeps task words out of flag parsing.
func TestCLISearchJSONSelectorsAgree(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		limit := strconv.Itoa(hegel.Draw(ht, hegel.Integers(1, 20)))
		out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", "--json", "--limit", limit, "--", query)
		selector := hegel.Draw(ht, hegel.SampledFrom([][]string{
			{"--format", "json"}, {"--format=json"}, {"--json=true"},
			{"--json=false", "--format", "json"}, {"--agent=false", "--json"},
			{"--json", "--format", "json", "--format=json"},
		}))
		position := hegel.Draw(ht, hegel.Integers(0, 2))
		parts := [][]string{{"cli"}, {"search"}, {"--limit", limit, "--", query}}
		parts[position] = append(selector, parts[position]...)
		args := append(append(parts[0], parts[1]...), parts[2]...)
		other, otherStderr, otherErr := executeRootCapture(t, t.Context(), args...)
		require.Equal(ht, out, other)
		require.Equal(ht, stderr, otherStderr)
		if err != nil {
			require.EqualError(ht, otherErr, err.Error())
		} else {
			require.NoError(ht, otherErr)
		}
	}, hegel.WithTestCases(1000))
}

func TestCLISearchTextAgreesWithJSON(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.OneOf[string](hegel.Text(), hegel.SampledFrom([]string{"issue", "show", "list", "comment"})))
		if strings.TrimSpace(query) == "" {
			return // blank-task rejection is covered by the usage-error contract
		}
		limit := strconv.Itoa(hegel.Draw(ht, hegel.Integers(1, 20)))
		out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", "--json", "--limit", limit, "--", query)
		require.NoError(ht, err, stderr)
		var response clisearch.Response
		require.NoError(ht, json.Unmarshal([]byte(out), &response))
		var want strings.Builder
		hint := "kata"
		for i, result := range response.Results {
			fmt.Fprintf(&want, "%s  %s\n", result.Command, result.Summary)
			if i == 0 {
				hint = result.Command
			}
		}
		fmt.Fprintf(&want, "Run '%s --help' for flags and examples.\n", hint)
		selector := hegel.Draw(ht, hegel.SampledFrom([][]string{nil, {"--format=human"}, {"--agent"}, {"--format", "agent"}}))
		args := append([]string{"cli", "search", "--limit", limit}, selector...)
		args = append(args, "--", query)
		text, stderr, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(ht, err, stderr)
		prefix := ""
		if len(selector) > 0 && selector[0] != "--format=human" {
			prefix = fmt.Sprintf("OK search results=%d\n", len(response.Results))
		}
		require.Equal(ht, prefix+want.String(), text)
	}, hegel.WithTestCases(1000))
}

func TestCLISearchResultsEligible(t *testing.T) {
	queries := make([]string, 0, len(cliSearchTasks))
	for _, task := range cliSearchTasks {
		queries = append(queries, task.query)
	}
	queries = append(queries, "forgejo", "attention", "contract", "browser", "tui", "serve", "completion", "help")
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.OneOf[string](hegel.Text(), hegel.SampledFrom(queries)))
		if strings.TrimSpace(query) == "" {
			return // blank-task rejection is a separate usage contract
		}
		out, stderr, err := executeRootCapture(t, t.Context(), "cli", "search", "--json", "--limit", "20", "--", query)
		require.NoError(ht, err, stderr)
		var response struct {
			Results []struct{ Command string } `json:"results"`
		}
		require.NoError(ht, json.Unmarshal([]byte(out), &response))
		root := newRootCmd()
		for _, result := range response.Results {
			cmd, rest, err := root.Find(strings.Fields(strings.TrimPrefix(result.Command, "kata ")))
			require.NoError(ht, err)
			require.Empty(ht, rest)
			require.True(ht, cmd.Runnable(), result.Command)
			for _, runner := range []string{"kata ui", "kata tui", "kata mcp serve", "kata connector serve", "kata agent-hook contract", "kata agent-hook attention", "kata attention-hook", "kata agent-contract-hook"} {
				require.False(ht, result.Command == runner || strings.HasPrefix(result.Command, runner+" "), result.Command)
			}
			for node := cmd; node != nil; node = node.Parent() {
				require.False(ht, node.Hidden, result.Command)
				require.Empty(ht, node.Deprecated, result.Command)
				require.NotContains(ht, []string{"help", "completion", "cli"}, node.Name(), result.Command)
			}
		}
	}, hegel.WithTestCases(1000))
}

func BenchmarkCLISearch(b *testing.B) {
	root := newRootCmd()
	opts := cliSearchOptions()
	b.ResetTimer()
	for b.Loop() {
		clisearch.Search(root, "add a comment to an issue", 5, opts)
	}
}

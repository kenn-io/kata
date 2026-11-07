package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"hegel.dev/go/hegel"

	katadocs "go.kenn.io/kata/docs"
	mcpserver "go.kenn.io/kata/internal/mcp"
)

// A guide is local, version-matched text, even when no daemon is reachable.
func TestCronGuideMatchesMCPWithoutDaemon(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	page, err := katadocs.Page("guide/cron-authoring.md")
	require.NoError(t, err)
	source, ok := mcpserver.SkillSource("cron")
	require.True(t, ok)
	require.Equal(t, page, source)
	markdown, ok := mcpserver.SkillMarkdown("cron")
	require.True(t, ok)
	require.Contains(t, markdown, page)
	for _, flag := range []string{"", "--json", "--agent"} {
		args := []string{"cron", "guide"}
		if flag != "" {
			args = append(args, flag)
		}
		out, diagnostics, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err, diagnostics)
		require.Equal(t, page, out, flag)
	}

	out, _, err := executeRootCapture(t, t.Context(), "cron", "guide", "unexpected")
	require.Error(t, err)
	require.NotContains(t, out, page)
}

type cronGuideErrorWriter struct {
	limit int
	body  strings.Builder
	err   error
}

func (w *cronGuideErrorWriter) Write(p []byte) (int, error) {
	n := min(len(p), max(0, w.limit-w.body.Len()))
	w.body.Write(p[:n])
	return n, w.err
}

// The command must return the output failure, including after partial delivery.
func TestCronGuidePropagatesOutputErrors(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SERVER", "http://127.0.0.1:1")
	page, err := katadocs.Page("guide/cron-authoring.md")
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		failure := errors.New(hegel.Draw(ht, hegel.Text()))
		writer := &cronGuideErrorWriter{limit: hegel.Draw(ht, hegel.Integers(0, len(page)-1)), err: failure}
		root := newRootCmd()
		root.SetArgs([]string{"cron", "guide"})
		root.SetOut(writer)
		root.SetErr(io.Discard)
		root.SilenceErrors, root.SilenceUsage = true, true
		require.ErrorIs(ht, root.ExecuteContext(t.Context()), failure)
		require.Equal(ht, page[:writer.limit], writer.body.String())
	}, hegel.WithTestCases(100))
}

func TestCronGuideIsDiscoverableInHelp(t *testing.T) {
	for _, args := range [][]string{{"cron", "--help"}, {"cron", "job", "create", "--help"}, {"cron", "job", "update", "--help"}} {
		out, diagnostics, err := executeRootCapture(t, t.Context(), args...)
		require.NoError(t, err, diagnostics)
		require.Contains(t, out, "kata cron guide")
	}
}

// Copied guide commands must parse using the real command tree and map to MCP.
func TestCronGuideCommandsExist(t *testing.T) {
	body, err := katadocs.Page("guide/cron-authoring.md")
	require.NoError(t, err)
	source := []byte(body)
	root := goldmark.DefaultParser().Parse(text.NewReader(source))
	var commands []string
	require.NoError(t, ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.CodeSpan:
			var span strings.Builder
			for child := node.FirstChild(); child != nil; child = child.NextSibling() {
				span.Write(child.(*ast.Text).Segment.Value(source))
			}
			if command := strings.ReplaceAll(span.String(), "\n", " "); strings.HasPrefix(command, "kata ") {
				commands = append(commands, command)
			}
		case *ast.FencedCodeBlock:
			if string(node.Language(source)) == "sh" {
				for line := range strings.SplitSeq(string(node.Lines().Value(source)), "\n") {
					if command := strings.TrimSpace(line); strings.HasPrefix(command, "kata ") {
						commands = append(commands, command)
					}
				}
			}
		}
		return ast.WalkContinue, nil
	}))
	require.NotEmpty(t, commands)
	var mapped []string
	for _, row := range mcpserver.SkillCommands() {
		mapped = append(mapped, row.CLI)
	}
	for _, command := range commands {
		require.NoError(t, validateHelpCommand(command), command)
		argv, err := splitHelpWords(command)
		require.NoError(t, err)
		cmd, _, err := newRootCmd().Find(argv[1:])
		require.NoError(t, err)
		require.Contains(t, mapped, cmd.CommandPath(), command)
	}
}

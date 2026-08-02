package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/markdownrender"
	"go.kenn.io/kata/internal/textsafe"
)

func TestShowMarkdownRendererHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_SHOW_MARKDOWN_HELPER") != "1" {
		return
	}
	marker := slices.Index(os.Args, "--")
	if marker < 0 || marker+1 >= len(os.Args) {
		os.Exit(20)
	}
	mode := os.Args[marker+1]
	switch mode {
	case "echo", "echo-newline":
		payload, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(21)
		}
		fmt.Printf("arg=%s env=%s input=%s", os.Args[marker+2], os.Getenv("SHOW_RENDER_ENV"), payload)
		if mode == "echo-newline" {
			fmt.Println()
		}
	case "fail":
		payload, _ := io.ReadAll(os.Stdin)
		_, _ = fmt.Fprintf(os.Stderr, "renderer rejected %s", payload)
		os.Exit(9)
	case "wait":
		select {}
	case "spawn-descendant":
		readyPath := os.Args[marker+2]
		//nolint:gosec // G204: this test starts its own fixed test binary helper with fixed arguments.
		child := exec.Command(
			os.Args[0], "-test.run=TestShowMarkdownRendererHelperProcess", "--", "wait",
		)
		child.Env = os.Environ()
		configureShowMarkdownHelperChild(child)
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(23)
		}
		//nolint:gosec // G703: readyPath is a test-owned path created under t.TempDir.
		if err := os.WriteFile(readyPath, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(24)
		}
		select {}
	default:
		os.Exit(22)
	}
	os.Exit(0)
}

func helperRenderer(mode string, extra ...string) *externalShowMarkdownRenderer {
	argv := []string{os.Args[0], "-test.run=TestShowMarkdownRendererHelperProcess", "--", mode}
	argv = append(argv, extra...)
	return &externalShowMarkdownRenderer{
		argv: argv, timeout: 500 * time.Millisecond, grace: 50 * time.Millisecond,
	}
}

func TestExternalShowMarkdownRendererPassesArgvEnvAndStdin(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	t.Setenv("SHOW_RENDER_ENV", "inherited")
	renderer := helperRenderer("echo", "argument with spaces")

	got, err := renderer.Render(context.Background(), markdownComment, "**hello**", 80)
	require.NoError(t, err)
	assert.Equal(t, "arg=argument with spaces env=inherited input=**hello**", got)
}

func TestExternalShowMarkdownRendererSanitizesStdin(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("echo", "argument")

	got, err := renderer.Render(
		context.Background(), markdownComment,
		"before\x1b[2Jafter\x1b]8;;https://evil.example/\x1b\\link\x1b]8;;\x1b\\\u202espoof&#27;[8mvisible\tok\nnext", 80,
	)
	require.NoError(t, err)
	assert.Equal(t, "arg=argument env= input=beforeafterlinkspoofvisible\tok\nnext", got)
}

func TestExternalShowMarkdownRendererNormalizesFinalNewlineAtReinsertion(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	var got [][]string
	for _, mode := range []string{"echo", "echo-newline"} {
		renderer := helperRenderer(mode, "argument")
		rendered, err := renderer.Render(context.Background(), markdownComment, "body", 80)
		require.NoError(t, err)
		got = append(got, markdownrender.ANSIWrappedLines(rendered, 80))
	}
	assert.Equal(t, got[0], got[1])
}

func TestExternalShowMarkdownRendererDiscardsStderr(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("fail")

	_, err := renderer.Render(context.Background(), markdownComment, "private body", 80)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "renderer")
	assert.Contains(t, err.Error(), "comment")
	assert.NotContains(t, err.Error(), "private body")
	assert.NotContains(t, err.Error(), "renderer rejected")
}

func TestExternalShowMarkdownRendererNamesMissingExecutable(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "missing-renderer")
	renderer := newExternalShowMarkdownRenderer([]string{executable})

	_, err := renderer.Render(context.Background(), markdownDescription, "private body", 80)
	require.Error(t, err)
	assert.Contains(t, err.Error(), strconv.Quote(executable))
	assert.Contains(t, err.Error(), "description")
	assert.NotContains(t, err.Error(), "private body")
}

func TestExternalShowMarkdownRendererTimesOutPerInvocation(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("wait")
	renderer.timeout = 50 * time.Millisecond

	started := time.Now()
	_, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}

func TestExternalShowMarkdownRendererPreservesParentCancellation(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("wait")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := renderer.Render(ctx, markdownDescription, "body", 80)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBuiltinShowMarkdownRendererUsesSharedStyle(t *testing.T) {
	rows := newRowRendererFor(colorprofile.ANSI256)
	got, err := rows.markdownRenderer().Render(
		context.Background(), markdownDescription, "## Steps", 80,
	)
	require.NoError(t, err)
	assert.Contains(t, textsafe.StripANSI(got), "Steps")
	assert.NotContains(t, got, "## Steps")

	var styled bytes.Buffer
	_, err = fmt.Fprint(rows.downsample(&styled), got)
	require.NoError(t, err)
	assert.Contains(t, styled.String(), "\x1b[")

	noColorRows := newRowRendererFor(colorprofile.NoTTY)
	plain, err := noColorRows.markdownRenderer().Render(
		context.Background(), markdownDescription, "## Steps", 80,
	)
	require.NoError(t, err)
	var noColor bytes.Buffer
	_, err = fmt.Fprint(noColorRows.downsample(&noColor), plain)
	require.NoError(t, err)
	assert.NotContains(t, noColor.String(), "\x1b[")
	assert.Equal(t, textsafe.StripANSI(styled.String()), noColor.String())
}

func TestConfiguredShowMarkdownRendererSelectsOverride(t *testing.T) {
	rows := newRowRendererFor(colorprofile.ANSI256)
	got := configuredShowMarkdownRenderer(config.DisplayConfig{
		MarkdownRenderer: []string{"renderer", "--flag"},
	}, rows)
	external, ok := got.(*externalShowMarkdownRenderer)
	require.True(t, ok)
	assert.Equal(t, []string{"renderer", "--flag"}, external.argv)
}

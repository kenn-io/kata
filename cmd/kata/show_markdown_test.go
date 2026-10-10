package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kit/tui/markdownrender"
)

func helperRenderer(t *testing.T, mode string, extra ...string) *externalShowMarkdownRenderer {
	t.Helper()
	argv := []string{externalFixtureBinary(t, "show-markdown-renderer"), "--", mode}
	argv = append(argv, extra...)
	// The default timeout only guards against hangs; it must leave the
	// spawn-descendant helper room to signal readiness under parallel test
	// load. Timeout-behavior tests override it explicitly.
	return &externalShowMarkdownRenderer{
		argv: argv, timeout: 5 * time.Second, grace: 50 * time.Millisecond,
	}
}

func TestExternalShowMarkdownRendererPassesArgvEnvAndStdin(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	t.Setenv("SHOW_RENDER_ENV", "inherited")
	renderer := helperRenderer(t, "echo", "argument with spaces")

	got, err := renderer.Render(context.Background(), markdownComment, "**hello**", 80)
	require.NoError(t, err)
	assert.Equal(t, "arg=argument with spaces env=inherited input=**hello**", got)
}

func TestExternalShowMarkdownRendererSanitizesStdin(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer(t, "echo", "argument")

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
		renderer := helperRenderer(t, mode, "argument")
		rendered, err := renderer.Render(context.Background(), markdownComment, "body", 80)
		require.NoError(t, err)
		got = append(got, markdownrender.ANSIWrappedLines(rendered, 80))
	}
	assert.Equal(t, got[0], got[1])
}

func TestExternalShowMarkdownRendererDiscardsStderr(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer(t, "fail")

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
	renderer := helperRenderer(t, "wait")
	renderer.timeout = 50 * time.Millisecond

	started := time.Now()
	_, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), time.Second)
}

func TestExternalShowMarkdownRendererPreservesParentCancellation(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer(t, "wait")
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

func TestBuiltinShowMarkdownRendererCodeBlockBackgroundFollowsColorMode(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	tests := []struct {
		name           string
		mode           string
		wantBackground string
	}{
		{name: "light", mode: "light", wantBackground: "252"},
		{name: "dark", mode: "dark", wantBackground: "236"},
		{name: "unknown", mode: "auto"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KATA_COLOR_MODE", tt.mode)
			rows := newRowRendererFor(colorprofile.ANSI256)
			rendered, err := rows.markdownRenderer().Render(
				context.Background(), markdownDescription, "```text\ncode\n```", 80,
			)
			require.NoError(t, err)

			var out bytes.Buffer
			_, err = fmt.Fprint(rows.downsample(&out), rendered)
			require.NoError(t, err)
			if tt.wantBackground == "" {
				assert.NotContains(t, out.String(), "\x1b[48;5;")
				return
			}
			assert.Contains(t, out.String(), "\x1b[48;5;"+tt.wantBackground+"m")
		})
	}
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

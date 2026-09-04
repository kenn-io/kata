package markdownrender

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/textsafe"
)

func TestRenderLinesSanitizesStoredControlSequences(t *testing.T) {
	lines, err := RenderLines(
		"## Steps\n\n- run `kata show`\x1b]2;unsafe\x07",
		Options{Width: 40},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")
	assert.Contains(t, textsafe.StripANSI(got), "Steps")
	assert.Contains(t, textsafe.StripANSI(got), "kata show")
	assert.NotContains(t, got, "unsafe")
	assert.NotContains(t, got, "## Steps")
}

func TestRenderLinesSanitizesDecodedControlEntities(t *testing.T) {
	lines, err := RenderLines(
		"**safe**&#27;]52;c;clipboard&#7;&#x202E;spoof",
		Options{Width: 40},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")
	assert.Contains(t, got, "\x1b[1m")
	assert.NotContains(t, got, "\x1b]52;")
	assert.NotContains(t, got, "\x07")
	assert.NotContains(t, got, "clipboard")
	assert.NotContains(t, got, "\u202e")
	assert.Contains(t, got, "spoof")
}

func TestRenderLinesRejectsDecodedConceal(t *testing.T) {
	lines, err := RenderLines(
		"before&#27;[8mvisible&#27;[31mred",
		Options{Width: 80},
	)
	require.NoError(t, err)
	got := strings.Join(lines, "\n")

	assert.Equal(t, "beforevisiblered", textsafe.StripANSI(got))
	assert.NotContains(t, got, "\x1b[8m")
	assert.NotContains(t, got, "\x1b[31m")
}

func TestTerminalRendererFormatsIssueMarkdown(t *testing.T) {
	background := "236"
	input := "## Steps\n\n" +
		"> Keep context\n\n" +
		"1. **Open** [issue](https://example.com)\n" +
		"2. [x] Comment with `kata comment`\n\n" +
		"```go\nfmt.Println(\"ok\")\n```\n\n" +
		"| Field | Value |\n| --- | --- |\n| Status | open |\n\n" +
		"![diagram](https://example.com/diagram.png)\n"
	got, err := renderMarkdownDocument(
		input, Options{Width: 80, CodeBlockBackground: &background},
	)
	require.NoError(t, err)

	want := `Steps
| Keep context

1. Open issue https://example.com
2. [x] Comment with ` + "`kata comment`" + `

fmt.Println("ok")

| Field | Value |
| --- | --- |
| Status | open |

[image: diagram] https://example.com/diagram.png`
	assert.Equal(t, want, textsafe.StripANSI(strings.TrimSpace(got)))
	assert.Contains(t, got, "\x1b[1mSteps\x1b[22m")
	assert.Contains(t, got, "issue \x1b[4mhttps://example.com\x1b[24m")
	assert.Contains(t, got, "\x1b[48;5;236mfmt.Println(\"ok\")\x1b[49m")
}

func TestTerminalRendererKeepsHeadingBoldAfterNestedStrong(t *testing.T) {
	got, err := renderMarkdownDocument(
		"## Before **nested** after\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "\x1b[1mBefore nested after\x1b[22m\n", got)
}

func TestTerminalRendererFormatsDefinitionLists(t *testing.T) {
	got, err := renderMarkdownDocument(
		"Term\n: Definition\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "Term\nDefinition\n", got)
}

func TestTerminalRendererKeepsLinkAndImageDestinationsVisible(t *testing.T) {
	lines, err := RenderLines(
		"[issue](https://example.com/issues/1)\n\n"+
			"![diagram](https://example.com/diagram.png)\n",
		Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t,
		"issue https://example.com/issues/1\n\n"+
			"[image: diagram] https://example.com/diagram.png",
		textsafe.StripANSI(strings.Join(lines, "\n")),
	)
}

func TestTerminalRendererDoesNotAddMailtoToAutolinkedEmail(t *testing.T) {
	got, err := renderMarkdownDocument(
		"<user@example.com>\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "user@example.com\n", got)
}

func TestTerminalRendererKeepsHTMLBlockText(t *testing.T) {
	got, err := renderMarkdownDocument(
		"<div>hello <strong>world</strong></div>\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "hello world\n", got)
}

func TestTerminalRendererUsesCheckboxAsTaskMarker(t *testing.T) {
	got, err := renderMarkdownDocument(
		"- [x] done\n- [ ] pending\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "[x] done\n[ ] pending\n", got)
}

func TestTerminalRendererPreservesOrderedTaskMarker(t *testing.T) {
	got, err := renderMarkdownDocument(
		"2. [x] done\n3. [ ] pending\n", Options{Width: 80},
	)
	require.NoError(t, err)
	assert.Equal(t, "2. [x] done\n3. [ ] pending\n", got)
}

func TestANSIWrappedLinesPreservesVisibleContent(t *testing.T) {
	rendered := "\x1b[31mカタabcdef\x1b[0m"
	lines := ANSIWrappedLines(rendered, 4)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 4)
	}
	assert.Equal(t, "カタabcdef", textsafe.StripANSI(strings.Join(lines, "")))
}

func TestANSIWrappedLinesAllowsOnlySGRControls(t *testing.T) {
	rendered := "\x1b[31mred\x1b[0m" +
		"\x1b]52;c;clipboard\x07" +
		"\x1b[2Jcleared" +
		"\x1bP1;2|dcs\x1b\\" +
		"\x1b_apc\x1b\\" +
		"\x07" +
		"\u009b2J" +
		"\u202e"

	got := strings.Join(ANSIWrappedLines(rendered, 80), "\n")

	assert.Equal(t, "\x1b[31mred\x1b[0mcleared2J", got)
}

func TestANSIWrappedLinesTerminatesUnclosedStyle(t *testing.T) {
	assert.Equal(t, []string{"\x1b[31mred\x1b[0m"}, ANSIWrappedLines("\x1b[31mred", 80))
}

func TestANSIWrappedLinesNormalizesOnlyOuterLineEndings(t *testing.T) {
	want := []string{"first", "", "second"}
	assert.Equal(t, want, ANSIWrappedLines("\nfirst\r\n\r\nsecond\r\n", 80))
	assert.Equal(t, want, ANSIWrappedLines("first\n\nsecond", 80))
}

func TestANSIWrappedLinesRemovesANSIOnlyOuterRowsWithoutLosingState(t *testing.T) {
	want := []string{"\x1b[31mfirst", "", "second\x1b[0m"}
	assert.Equal(t, want, ANSIWrappedLines("\x1b[31m\nfirst\n\nsecond\n\x1b[0m", 80))
}

func TestANSIWrappedLinesTrimsLastVisibleRowBeforeTrailingANSIState(t *testing.T) {
	assert.Equal(t, []string{"text\x1b[0m"}, ANSIWrappedLines("text   \n\x1b[0m", 80))
}

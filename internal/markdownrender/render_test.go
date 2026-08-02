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

func TestANSIWrappedLinesPreservesVisibleContent(t *testing.T) {
	rendered := "\x1b[31mカタabcdef\x1b[0m"
	lines := ANSIWrappedLines(rendered, 4)
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.LessOrEqual(t, ansi.StringWidth(line), 4)
	}
	assert.Equal(t, "カタabcdef", textsafe.StripANSI(strings.Join(lines, "")))
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

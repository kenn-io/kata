package planesync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDescriptionMarkdownPreservesRichContent(t *testing.T) {
	input := "<p>Text &amp; Unicode café.</p><ol><li>First<ul><li>Nested</li></ul></li></ol><blockquote>Quote</blockquote><p><del>Old</del><img src=\"/image.png\" alt=\"Diagram\"></p><table><tr><th>Key</th><th>Value</th></tr><tr><td>example</td><td>42</td></tr></table><p><a href=\"../other\">Relative</a></p><script>unsafe code</script>"
	got, err := descriptionMarkdown(input, "https://plane.example/example-workspace/projects/example/issues/example/")
	require.NoError(t, err)
	for _, text := range []string{"Unicode café", "First", "Nested", "> Quote", "~~Old~~", "![Diagram](https://plane.example/image.png)", "Key", "Value", "example", "42", "[Relative](https://plane.example/example-workspace/projects/example/issues/other)"} {
		require.Contains(t, got, text)
	}
	require.NotContains(t, got, "unsafe code")
}

func TestDescriptionRejectsExpansionAndMalformedText(t *testing.T) {
	for _, input := range []string{strings.Repeat("*", 600000), "<p>\x00</p>", "<p>\xff</p>", strings.Repeat("<div>", 300) + "Deep" + strings.Repeat("</div>", 300)} {
		_, err := descriptionMarkdown(input, "https://plane.example/")
		require.Error(t, err)
	}
}

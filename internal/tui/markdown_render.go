package tui

import (
	"strings"

	"charm.land/glamour/v2"
	glamansi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
)

func renderMarkdownLines(markdown string, width int) []string {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	rendered, ok := renderMarkdown(markdown, width)
	if !ok {
		return wrapBody(sanitizeForDisplay(markdown), width)
	}
	rendered = strings.TrimRight(rendered, "\n")
	if rendered == "" {
		return nil
	}
	raw := strings.Split(rendered, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, " ")
		// Glamour word-wraps prose to `width` but leaves preformatted
		// content (code blocks, long URLs, table cells, stack traces)
		// at its natural width. Truncating those would silently drop
		// content the user has no way to scroll back to. Hardwrap
		// instead so every byte stays on screen across multiple rows.
		// preserveSpace=true keeps leading indentation on continued
		// rows so wrapped code stays readable.
		lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
	}
	return lines
}

func renderMarkdown(markdown string, width int) (out string, ok bool) {
	if width < 1 {
		width = 1
	}
	defer func() {
		if recover() != nil {
			out = ""
			ok = false
		}
	}()
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(markdownStyleConfig()),
		glamour.WithPreservedNewLines(),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
	)
	if err != nil {
		return "", false
	}
	out, err = r.Render(sanitizeForDisplay(markdown))
	if err != nil {
		return "", false
	}
	if activeColorMode == colorNone {
		out = stripANSI(out)
	}
	return out, true
}

func markdownStyleConfig() glamansi.StyleConfig {
	bold := true
	italic := true
	zero := uint(0)
	quoteToken := "| "
	codeBackground := markdownCodeBlockBackground()
	return glamansi.StyleConfig{
		Document: glamansi.StyleBlock{Margin: &zero},
		BlockQuote: glamansi.StyleBlock{
			Indent:      &zero,
			IndentToken: &quoteToken,
		},
		Paragraph: glamansi.StyleBlock{Margin: &zero},
		Heading: glamansi.StyleBlock{
			StylePrimitive: glamansi.StylePrimitive{Bold: &bold},
			Margin:         &zero,
		},
		H1:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H2:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H3:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H4:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H5:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		H6:     glamansi.StyleBlock{StylePrimitive: glamansi.StylePrimitive{Bold: &bold}},
		Strong: glamansi.StylePrimitive{Bold: &bold},
		Emph:   glamansi.StylePrimitive{Italic: &italic},
		Item:   glamansi.StylePrimitive{BlockPrefix: "- "},
		Enumeration: glamansi.StylePrimitive{
			BlockPrefix: ". ",
		},
		Task: glamansi.StyleTask{Ticked: "[x] ", Unticked: "[ ] "},
		Link: glamansi.StylePrimitive{Underline: &bold},
		Code: glamansi.StyleBlock{
			StylePrimitive: glamansi.StylePrimitive{Prefix: "`", Suffix: "`"},
		},
		CodeBlock: glamansi.StyleCodeBlock{
			StyleBlock: glamansi.StyleBlock{
				StylePrimitive: glamansi.StylePrimitive{
					BackgroundColor: codeBackground,
				},
				Margin: &zero,
			},
		},
		HorizontalRule: glamansi.StylePrimitive{Format: ""},
		ImageText: glamansi.StylePrimitive{
			Format: "[image: {{.text}}]",
		},
		List: glamansi.StyleList{
			StyleBlock:  glamansi.StyleBlock{Margin: &zero},
			LevelIndent: 2,
		},
	}
}

func markdownCodeBlockBackground() *string {
	if markdownCodeBlockBg == "" {
		return nil
	}
	s := markdownCodeBlockBg
	return &s
}

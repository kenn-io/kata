// Package markdownrender renders user-authored Markdown for terminal display.
package markdownrender

import (
	"fmt"
	"strings"

	"charm.land/glamour/v2"
	glamansi "charm.land/glamour/v2/ansi"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/textsafe"
)

type Options struct {
	Width               int
	CodeBlockBackground *string
}

func Render(markdown string, opts Options) (out string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = ""
			err = fmt.Errorf("render markdown: %v", recovered)
		}
	}()
	width := max(1, opts.Width)
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(styleConfig(opts.CodeBlockBackground)),
		glamour.WithPreservedNewLines(),
		glamour.WithWordWrap(width),
		glamour.WithTableWrap(true),
	)
	if err != nil {
		return "", err
	}
	return renderer.Render(textsafe.Block(markdown))
}

func RenderLines(markdown string, opts Options) ([]string, error) {
	if strings.TrimSpace(markdown) == "" {
		return nil, nil
	}
	rendered, err := Render(markdown, opts)
	if err != nil {
		return nil, err
	}
	return ANSIWrappedLines(rendered, opts.Width), nil
}

func ANSIWrappedLines(rendered string, width int) []string {
	rendered = strings.ReplaceAll(rendered, "\r\n", "\n")
	rendered = strings.ReplaceAll(rendered, "\r", "\n")
	rendered = strings.Trim(rendered, "\n")
	if rendered == "" {
		return nil
	}
	width = max(1, width)
	raw := strings.Split(rendered, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimRight(line, " ")
		// Glamour word-wraps prose to width but leaves preformatted content
		// (code blocks, long URLs, table cells, and stack traces) at its natural
		// width. Hardwrap preserves all content across display rows.
		lines = append(lines, strings.Split(ansi.Hardwrap(line, width, true), "\n")...)
	}
	return lines
}

func styleConfig(codeBackground *string) glamansi.StyleConfig {
	bold := true
	italic := true
	zero := uint(0)
	quoteToken := "| "
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

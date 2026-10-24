package cron

import (
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kata/internal/colormode"
	"go.kenn.io/kit/tui/markdownrender"
)

func renderMarkdownLines(markdown string, width int) []string {
	var background *string
	if markdownCodeBlockBg != "" {
		background = &markdownCodeBlockBg
	}
	lines, err := markdownrender.RenderLines(markdown, markdownrender.Options{Width: width, CodeBlockBackground: background})
	if err != nil {
		return wrapText(markdown, width)
	}
	if activeColorMode == colormode.None {
		for i := range lines {
			lines[i] = ansi.Strip(lines[i])
		}
	}
	return lines
}

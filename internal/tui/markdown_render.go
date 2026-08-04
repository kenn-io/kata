package tui

import (
	"go.kenn.io/kata/internal/markdownrender"
)

func renderMarkdownLines(markdown string, width int) []string {
	lines, err := markdownrender.RenderLines(markdown, markdownrender.Options{
		Width:               width,
		CodeBlockBackground: markdownCodeBlockBackground(),
	})
	if err != nil {
		return wrapBody(sanitizeForDisplay(markdown), width)
	}
	if activeColorMode == colorNone {
		for i := range lines {
			lines[i] = stripANSI(lines[i])
		}
	}
	return lines
}

func markdownCodeBlockBackground() *string {
	if markdownCodeBlockBg == "" {
		return nil
	}
	s := markdownCodeBlockBg
	return &s
}
